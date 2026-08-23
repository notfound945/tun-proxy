package status

import (
	"context"
	"encoding/json"
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
	expiresAt := started.Add(time.Hour)
	server, err := StartWithOptions(path, func(options QueryOptions) Snapshot {
		snapshot := Snapshot{
			PID: 42, StartedAt: started, ConfigDigest: "sha256:test",
			Limits: Limits{FakeIPv6Mappings: 2048}, FakeIPv6: fakeip.Stats{Capacity: 65_536, Limit: 2048, Used: 12},
		}
		if options.IncludeFakeIPMappings {
			snapshot.FakeIPMappings = NewMappingSet(
				[]fakeip.Mapping{{Domain: "example.com", Address: "198.18.0.10", ExpiresAt: expiresAt}},
				[]fakeip.Mapping{{Domain: "example.com", Address: "fd00:7::a", ExpiresAt: expiresAt}},
			)
		}
		return snapshot
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
	if snapshot.FakeIPMappings != nil {
		t.Fatalf("default query unexpectedly included mappings: %+v", snapshot.FakeIPMappings)
	}
	detailed, err := QueryWithOptions(t.Context(), path, QueryOptions{IncludeFakeIPMappings: true})
	if err != nil {
		t.Fatal(err)
	}
	if detailed.FakeIPMappings == nil || len(detailed.FakeIPMappings.IPv4) != 1 || len(detailed.FakeIPMappings.IPv6) != 1 {
		t.Fatalf("detailed mappings = %+v", detailed.FakeIPMappings)
	}
	if detailed.FakeIPMappings.IPv4[0].Domain != "example.com" || detailed.FakeIPMappings.IPv4[0].Address != "198.18.0.10" {
		t.Fatalf("IPv4 mapping = %+v", detailed.FakeIPMappings.IPv4[0])
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

func TestMappingSetJSONUsesStableLowercaseFields(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	encoded, err := json.Marshal(NewMappingSet([]fakeip.Mapping{{
		Domain: "example.com", Address: "198.18.0.10", ExpiresAt: expiresAt,
	}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ipv4":[{"domain":"example.com","address":"198.18.0.10","expires_at":"2026-08-23T12:00:00Z"}],"ipv6":[]}`
	if string(encoded) != want {
		t.Fatalf("JSON = %s, want %s", encoded, want)
	}
}

func TestServerSupportsLegacyClientWithoutRequest(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "tp-status-legacy-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "status.sock")
	server, err := Start(path, func() Snapshot { return Snapshot{PID: 42} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})

	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // Best-effort test cleanup.
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	var snapshot Snapshot
	if err := json.NewDecoder(connection).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != Version || snapshot.PID != 42 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestQuerySupportsLegacyServer(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "tp-status-legacy-server-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "status.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // Best-effort test cleanup.
	serverErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer connection.Close() //nolint:errcheck // Best-effort test cleanup.
		serverErr <- json.NewEncoder(connection).Encode(Snapshot{Version: Version, PID: 42})
	}()

	snapshot, err := QueryWithOptions(t.Context(), path, QueryOptions{IncludeFakeIPMappings: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if snapshot.PID != 42 || snapshot.FakeIPMappings != nil {
		t.Fatalf("snapshot = %+v", snapshot)
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
