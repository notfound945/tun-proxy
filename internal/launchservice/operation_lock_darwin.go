//go:build darwin

package launchservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const operationMetadataVersion = 1
const maxOperationMetadataSize = 16 << 10

var ErrOperationBusy = errors.New("service operation is already in progress")

type OperationKind string

const (
	OperationInstall        OperationKind = "install"
	OperationStart          OperationKind = "start"
	OperationStop           OperationKind = "stop"
	OperationRestart        OperationKind = "restart"
	OperationSyncUserConfig OperationKind = "sync-user-config"
	OperationReload         OperationKind = "reload"
	OperationUpgrade        OperationKind = "upgrade"
	OperationUninstall      OperationKind = "uninstall"
	OperationCleanup        OperationKind = "cleanup"
)

type OperationSpec struct {
	Kind                 OperationKind
	ExpectedConfigDigest string
}

type OperationMetadata struct {
	Version              int           `json:"version"`
	ID                   string        `json:"id"`
	Kind                 OperationKind `json:"kind"`
	PID                  int           `json:"pid"`
	StartedAt            time.Time     `json:"started_at"`
	ExpectedConfigDigest string        `json:"expected_config_digest,omitempty"`
}

// OperationBusyError reports the best-effort metadata left by the process that
// currently owns the operation lock. Lock ownership is determined solely by
// flock; Holder may be nil when metadata is absent, stale, or malformed.
type OperationBusyError struct {
	Holder *OperationMetadata
}

func (err *OperationBusyError) Error() string {
	if err != nil && err.Holder != nil {
		return fmt.Sprintf("%s: operation=%s operation_id=%s pid=%d started_at=%s",
			ErrOperationBusy, err.Holder.Kind, err.Holder.ID, err.Holder.PID,
			err.Holder.StartedAt.UTC().Format(time.RFC3339))
	}
	return ErrOperationBusy.Error()
}

func (*OperationBusyError) Unwrap() error { return ErrOperationBusy }

type OperationGuard struct {
	Metadata OperationMetadata

	mutex sync.Mutex
	file  *os.File
}

// BeginOperation acquires the process-shared lock that serializes complete
// managed-service mutations. The lock is deliberately separate from the
// daemon instance lock and remains held until the returned guard is closed.
func (manager *Manager) BeginOperation(ctx context.Context, spec OperationSpec) (*OperationGuard, error) {
	if manager == nil {
		return nil, errors.New("service manager is required")
	}
	if ctx == nil {
		return nil, errors.New("service operation context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validOperationKind(spec.Kind) {
		return nil, fmt.Errorf("invalid service operation kind %q", spec.Kind)
	}
	if err := manager.validate(); err != nil {
		return nil, err
	}
	if err := manager.requireRoot(); err != nil {
		return nil, err
	}
	path := manager.Layout.OperationLock
	if err := validateOperationLockParent(path, manager.OwnerUID); err != nil {
		return nil, err
	}

	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open service operation lock %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()

	if err := validateOperationLockFile(path, file, manager.OwnerUID); err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			holder := readOperationMetadata(file)
			return nil, &OperationBusyError{Holder: holder}
		}
		return nil, fmt.Errorf("lock service operation file %q: %w", path, err)
	}

	metadata, err := newOperationMetadata(spec)
	if err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	if err := writeOperationMetadata(file, metadata); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	closeOnError = false
	return &OperationGuard{Metadata: metadata, file: file}, nil
}

func validOperationKind(kind OperationKind) bool {
	switch kind {
	case OperationInstall, OperationStart, OperationStop, OperationRestart,
		OperationSyncUserConfig, OperationReload, OperationUpgrade,
		OperationUninstall, OperationCleanup:
		return true
	default:
		return false
	}
}

func validateOperationLockParent(path string, ownerUID int) error {
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect service operation lock directory %q: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("service operation lock parent %q is not a directory", parent)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect owner of service operation lock directory %q", parent)
	}
	if stat.Uid != uint32(ownerUID) {
		return fmt.Errorf("refuse service operation lock directory %q owned by UID %d, want %d", parent, stat.Uid, ownerUID)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("refuse writable service operation lock directory %q with mode %04o", parent, info.Mode().Perm())
	}
	return nil
}

func validateOperationLockFile(path string, file *os.File, ownerUID int) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect service operation lock %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service operation lock %q is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("refuse service operation lock %q with mode %04o, want 0600", path, info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect owner of service operation lock %q", path)
	}
	if stat.Uid != uint32(ownerUID) {
		return fmt.Errorf("refuse service operation lock %q owned by UID %d, want %d", path, stat.Uid, ownerUID)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("refuse service operation lock %q with link count %d", path, stat.Nlink)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect service operation lock path %q: %w", path, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(pathInfo, info) {
		return fmt.Errorf("service operation lock path %q changed while opening", path)
	}
	return nil
}

func newOperationMetadata(spec OperationSpec) (OperationMetadata, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return OperationMetadata{}, fmt.Errorf("generate service operation ID: %w", err)
	}
	return OperationMetadata{
		Version:              operationMetadataVersion,
		ID:                   hex.EncodeToString(idBytes),
		Kind:                 spec.Kind,
		PID:                  os.Getpid(),
		StartedAt:            time.Now().UTC(),
		ExpectedConfigDigest: spec.ExpectedConfigDigest,
	}, nil
}

func writeOperationMetadata(file *os.File, metadata OperationMetadata) error {
	contents, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode service operation metadata: %w", err)
	}
	contents = append(contents, '\n')
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate service operation metadata: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek service operation metadata: %w", err)
	}
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write service operation metadata: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync service operation metadata: %w", err)
	}
	return nil
}

func readOperationMetadata(file *os.File) *OperationMetadata {
	contents := make([]byte, maxOperationMetadataSize+1)
	read, err := file.ReadAt(contents, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	if read == 0 || read > maxOperationMetadataSize {
		return nil
	}
	var metadata OperationMetadata
	if err := json.Unmarshal(contents[:read], &metadata); err != nil {
		return nil
	}
	if metadata.Version != operationMetadataVersion || metadata.ID == "" ||
		!validOperationKind(metadata.Kind) || metadata.PID <= 0 || metadata.StartedAt.IsZero() {
		return nil
	}
	return &metadata
}

// Close releases the service operation lock. The inode and its last metadata
// intentionally remain in place; only an actually held flock denotes an
// operation in flight.
func (guard *OperationGuard) Close() error {
	if guard == nil {
		return nil
	}
	guard.mutex.Lock()
	defer guard.mutex.Unlock()
	if guard.file == nil {
		return nil
	}
	var failures []error
	if err := unix.Flock(int(guard.file.Fd()), unix.LOCK_UN); err != nil {
		failures = append(failures, fmt.Errorf("unlock service operation: %w", err))
	}
	if err := guard.file.Close(); err != nil {
		failures = append(failures, fmt.Errorf("close service operation lock: %w", err))
	}
	guard.file = nil
	return errors.Join(failures...)
}
