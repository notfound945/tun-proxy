// Package system is the only package allowed to persist or mutate macOS
// network state.
package system

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const StateVersion = 1

type State struct {
	Version       int          `json:"version"`
	PID           int          `json:"pid"`
	StartedAt     time.Time    `json:"started_at"`
	ConfigDigest  string       `json:"config_digest"`
	Phase         string       `json:"phase"`
	LockFile      string       `json:"lock_file,omitempty"`
	TUNName       string       `json:"tun_name,omitempty"`
	StatusSocket  string       `json:"status_socket,omitempty"`
	ControlSocket string       `json:"control_socket,omitempty"`
	Route         *RouteState  `json:"route,omitempty"`
	Routes        []RouteState `json:"routes,omitempty"`
	DNS           []DNSState   `json:"dns,omitempty"`
}

type RouteState struct {
	Prefix    string `json:"prefix"`
	Interface string `json:"interface"`
	Gateway   string `json:"gateway,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

type DNSState struct {
	Service  string   `json:"service"`
	Previous []string `json:"previous"`
	Applied  []string `json:"applied"`
}

func NewState(configDigest string) State {
	return State{
		Version:      StateVersion,
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
		ConfigDigest: configDigest,
		Phase:        "starting",
	}
}

// WriteState atomically replaces path with a root-readable-only JSON file.
// It fsyncs both the file and containing directory so the recovery record is
// durable before a network mutation begins.
func WriteState(path string, state State) error {
	if err := validateState(state); err != nil {
		return err
	}
	if err := ensureSafeTarget(path); err != nil {
		return err
	}

	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".tun-proxy-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod temporary state file: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	removeTemporary = false

	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	defer dir.Close() //nolint:errcheck // Best-effort cleanup.
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func ReadState(path string) (State, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return State{}, fmt.Errorf("inspect state file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("state path %q is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return State{}, fmt.Errorf("state file %q permissions are %04o, want 0600 or stricter", path, info.Mode().Perm())
	}
	if info.Size() > 1<<20 {
		return State{}, fmt.Errorf("state file %q exceeds 1 MiB", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Geteuid()) {
		return State{}, fmt.Errorf("state file %q is owned by UID %d, want %d", path, stat.Uid, os.Geteuid())
	}

	file, err := os.Open(path)
	if err != nil {
		return State{}, fmt.Errorf("open state file: %w", err)
	}
	defer file.Close() //nolint:errcheck // Best-effort cleanup.
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return State{}, errors.New("decode state: trailing data is not allowed")
	}
	if err := validateState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func RemoveState(path string) error {
	_, err := ReadState(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("validate state file before removal: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove state file: %w", err)
	}
	return nil
}

func validateState(state State) error {
	if state.Version != StateVersion {
		return fmt.Errorf("state version must be %d, got %d", StateVersion, state.Version)
	}
	if state.PID <= 0 {
		return fmt.Errorf("state PID must be positive, got %d", state.PID)
	}
	if state.StartedAt.IsZero() {
		return errors.New("state start time is required")
	}
	if state.ConfigDigest == "" {
		return errors.New("state config digest is required")
	}
	if state.Phase == "" {
		return errors.New("state phase is required")
	}
	if state.StatusSocket != "" && (!filepath.IsAbs(state.StatusSocket) || filepath.Clean(state.StatusSocket) != state.StatusSocket) {
		return fmt.Errorf("status socket must be a clean absolute path: %q", state.StatusSocket)
	}
	if state.ControlSocket != "" && (!filepath.IsAbs(state.ControlSocket) || filepath.Clean(state.ControlSocket) != state.ControlSocket) {
		return fmt.Errorf("control socket must be a clean absolute path: %q", state.ControlSocket)
	}
	return nil
}

func ensureSafeTarget(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("state path must be a clean absolute path: %q", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect state target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("state target %q is not a regular file", path)
	}
	return nil
}
