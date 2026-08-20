// Package status exposes read-only runtime snapshots over a root-only Unix
// socket. The recovery state records the socket path so the CLI never guesses
// which process it is querying.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/hailinpan/tun-proxy/internal/fakedns"
	"github.com/hailinpan/tun-proxy/internal/fakeip"
	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/session"
	internaltun "github.com/hailinpan/tun-proxy/internal/tun"
)

const Version = 1

type Snapshot struct {
	Version      int               `json:"version"`
	PID          int               `json:"pid"`
	StartedAt    time.Time         `json:"started_at"`
	CapturedAt   time.Time         `json:"captured_at"`
	ConfigDigest string            `json:"config_digest"`
	Reload       ReloadStats       `json:"reload"`
	Network      NetworkStats      `json:"network"`
	Limits       Limits            `json:"limits"`
	Resources    ResourceStats     `json:"resources"`
	TUN          internaltun.Stats `json:"tun"`
	Netstack     netstack.Stats    `json:"netstack"`
	TCP          session.Stats     `json:"tcp"`
	UDP          session.UDPStats  `json:"udp"`
	DNS          fakedns.Stats     `json:"dns"`
	FakeIP       fakeip.Stats      `json:"fake_ip"`
	FakeIPv6     fakeip.Stats      `json:"fake_ipv6"`
	IPv6         IPv6Status        `json:"ipv6"`
}

type IPv6Status struct {
	Configured     bool   `json:"configured"`
	Enabled        bool   `json:"enabled"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}

type ReloadStats struct {
	Successes   uint64    `json:"successes"`
	Failures    uint64    `json:"failures"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

type NetworkStats struct {
	Refreshes   uint64    `json:"refreshes"`
	Failures    uint64    `json:"failures"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

type Limits struct {
	TCPFlows             int `json:"tcp_flows"`
	UDPSessions          int `json:"udp_sessions"`
	UDPSessionsPerSource int `json:"udp_sessions_per_source"`
	DNSConcurrent        int `json:"dns_concurrent"`
	FakeIPMappings       int `json:"fake_ip_mappings"`
	FakeIPv6Mappings     int `json:"fake_ipv6_mappings"`
	PacketQueue          int `json:"packet_queue"`
	PacketBuffers        int `json:"packet_buffers"`
}

type ResourceStats struct {
	Goroutines  uint64 `json:"goroutines"`
	OpenFDs     uint64 `json:"open_fds"`
	OpenFDError string `json:"open_fd_error,omitempty"`
	HeapAlloc   uint64 `json:"heap_alloc_bytes"`
	HeapInUse   uint64 `json:"heap_in_use_bytes"`
	StackInUse  uint64 `json:"stack_in_use_bytes"`
	Sys         uint64 `json:"sys_bytes"`
	NumGC       uint32 `json:"gc_cycles"`
}

func Resources() ResourceStats {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	result := ResourceStats{
		Goroutines: uint64(runtime.NumGoroutine()), HeapAlloc: memory.HeapAlloc,
		HeapInUse: memory.HeapInuse, StackInUse: memory.StackInuse, Sys: memory.Sys, NumGC: memory.NumGC,
	}
	openFDs, err := openFDCount()
	if err != nil {
		result.OpenFDError = err.Error()
	} else {
		result.OpenFDs = openFDs
	}
	return result
}

type Server struct {
	path      string
	listener  net.Listener
	snapshot  func() Snapshot
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func Start(path string, snapshot func() Snapshot) (*Server, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("status socket must be a clean absolute path: %q", path)
	}
	if snapshot == nil {
		return nil, errors.New("status snapshot function is required")
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("status socket %q already exists; run cleanup first", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect status socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen status socket %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("chmod status socket: %w", err)
	}
	server := &Server{path: path, listener: listener, snapshot: snapshot, done: make(chan struct{})}
	go server.serve()
	return server, nil
}

func (server *Server) serve() {
	defer close(server.done)
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			return
		}
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		snapshot := server.snapshot()
		if snapshot.Version == 0 {
			snapshot.Version = Version
		}
		if snapshot.CapturedAt.IsZero() {
			snapshot.CapturedAt = time.Now().UTC()
		}
		_ = json.NewEncoder(connection).Encode(snapshot)
		_ = connection.Close()
	}
}

func (server *Server) Close(ctx context.Context) error {
	server.closeOnce.Do(func() {
		server.closeErr = server.listener.Close()
		select {
		case <-server.done:
		case <-ctx.Done():
			server.closeErr = errors.Join(server.closeErr, ctx.Err())
		}
		if info, err := os.Lstat(server.path); err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				server.closeErr = errors.Join(server.closeErr, fmt.Errorf("refuse to remove non-socket status path %q", server.path))
			} else if err := os.Remove(server.path); err != nil {
				server.closeErr = errors.Join(server.closeErr, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			server.closeErr = errors.Join(server.closeErr, err)
		}
	})
	return server.closeErr
}

func Query(ctx context.Context, path string) (Snapshot, error) {
	dialer := net.Dialer{Timeout: time.Second}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("connect status socket: %w", err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	decoder := json.NewDecoder(connection)
	decoder.DisallowUnknownFields()
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode runtime status: %w", err)
	}
	if snapshot.Version != Version {
		return Snapshot{}, fmt.Errorf("runtime status version must be %d, got %d", Version, snapshot.Version)
	}
	return snapshot, nil
}

func RemoveStale(path string) error {
	return RemoveStaleForOwners(path, uint32(os.Geteuid()))
}

// RemoveStaleForOwners removes a Unix status socket only when its owner is in
// the explicit allowlist. This lets a root supervisor clean up a socket that
// was created by its dedicated non-root worker without accepting arbitrary
// filesystem objects from other users.
func RemoveStaleForOwners(path string, allowedUIDs ...uint32) error {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect stale status socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refuse to remove non-socket status path %q", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect status socket owner %q", path)
	}
	allowed := false
	for _, uid := range allowedUIDs {
		if stat.Uid == uid {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("status socket %q is owned by UID %d, not an allowed owner", path, stat.Uid)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale status socket: %w", err)
	}
	return nil
}
