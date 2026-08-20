package status

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/fakeip"
)

func TestServerRoundTripAndCleanup(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "tun-proxy-status-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "status.sock")
	started := time.Now().UTC()
	server, err := Start(path, func() Snapshot {
		return Snapshot{
			PID: 42, StartedAt: started, ConfigDigest: "sha256:test",
			Limits: Limits{FakeIPv6Mappings: 2048}, FakeIPv6: fakeip.Stats{Capacity: 65_536, Limit: 2048, Used: 12},
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v", info.Mode())
	}
	snapshot, err := Query(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PID != 42 || snapshot.ConfigDigest != "sha256:test" || snapshot.CapturedAt.IsZero() || snapshot.FakeIPv6.Used != 12 || snapshot.Limits.FakeIPv6Mappings != 2048 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remained after close: %v", err)
	}
}

func TestResourcesReportsOpenFileDescriptors(t *testing.T) {
	resources := Resources()
	if resources.OpenFDs < 3 {
		t.Fatalf("open FDs = %d, want at least stdin/stdout/stderr", resources.OpenFDs)
	}
}

func TestRemoveStaleForOwnersUsesExplicitAllowlist(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "tun-proxy-status-stale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "status.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	if err := RemoveStaleForOwners(path, uint32(os.Geteuid()+1)); err == nil || !strings.Contains(err.Error(), "not an allowed owner") {
		t.Fatalf("RemoveStaleForOwners() error = %v", err)
	}
	if err := RemoveStaleForOwners(path, uint32(os.Geteuid())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("status socket remains: %v", err)
	}
}
