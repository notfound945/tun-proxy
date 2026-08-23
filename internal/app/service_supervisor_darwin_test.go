//go:build darwin

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
)

func TestLoadManagedServiceConfigPreservesExactBytes(t *testing.T) {
	layout, contents := writeManagedServiceConfig(t, 0o600)
	loaded, runtime, digest, err := loadManagedServiceConfigForOwner(layout.Config, layout, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, contents) {
		t.Fatal("managed config bytes changed while loading")
	}
	wantRuntime, wantDigest, err := config.LoadBytesWithDigest(contents)
	if err != nil {
		t.Fatal(err)
	}
	if digest != wantDigest {
		t.Fatalf("digest = %q, want %q", digest, wantDigest)
	}
	if runtime.System.StateFile != wantRuntime.System.StateFile || runtime.FakeIP.PersistenceFile != wantRuntime.FakeIP.PersistenceFile {
		t.Fatalf("loaded runtime paths differ: got state=%q fake_ipv4=%q", runtime.System.StateFile, runtime.FakeIP.PersistenceFile)
	}
}

func TestLoadManagedServiceConfigRejectsUnsafeFiles(t *testing.T) {
	t.Run("wrong mode", func(t *testing.T) {
		layout, _ := writeManagedServiceConfig(t, 0o644)
		_, _, _, err := loadManagedServiceConfigForOwner(layout.Config, layout, uint32(os.Geteuid()))
		if err == nil || !strings.Contains(err.Error(), "mode 0600") {
			t.Fatalf("loadManagedServiceConfigForOwner() error = %v", err)
		}
	})

	t.Run("wrong owner", func(t *testing.T) {
		layout, _ := writeManagedServiceConfig(t, 0o600)
		_, _, _, err := loadManagedServiceConfigForOwner(layout.Config, layout, uint32(os.Geteuid()+1))
		if err == nil || !strings.Contains(err.Error(), "UID") {
			t.Fatalf("loadManagedServiceConfigForOwner() error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		layout, contents := newServiceSupervisorTestLayout(t)
		target := layout.Config + ".target"
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, layout.Config); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := loadManagedServiceConfigForOwner(layout.Config, layout, uint32(os.Geteuid())); err == nil {
			t.Fatal("managed config symlink was accepted")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		layout, _ := newServiceSupervisorTestLayout(t)
		if err := os.MkdirAll(filepath.Dir(layout.Config), 0o755); err != nil {
			t.Fatal(err)
		}
		contents := bytes.Repeat([]byte{'x'}, privsep.MaxConfigSize+1)
		if err := os.WriteFile(layout.Config, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := loadManagedServiceConfigForOwner(layout.Config, layout, uint32(os.Geteuid()))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("loadManagedServiceConfigForOwner() error = %v", err)
		}
	})
}

func TestLoadManagedServiceConfigRejectsUnmanagedPaths(t *testing.T) {
	layout, contents := newServiceSupervisorTestLayout(t)
	contents = bytes.ReplaceAll(contents, []byte(layout.State), []byte(layout.State+".other"))
	if err := os.MkdirAll(filepath.Dir(layout.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Config, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := loadManagedServiceConfigForOwner(layout.Config, layout, uint32(os.Geteuid()))
	if err == nil || !strings.Contains(err.Error(), "system.state_file") {
		t.Fatalf("loadManagedServiceConfigForOwner() error = %v", err)
	}
}

func writeManagedServiceConfig(t *testing.T, mode os.FileMode) (launchservice.Layout, []byte) {
	t.Helper()
	layout, contents := newServiceSupervisorTestLayout(t)
	if err := os.MkdirAll(filepath.Dir(layout.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Config, contents, mode); err != nil {
		t.Fatal(err)
	}
	return layout, contents
}

func newServiceSupervisorTestLayout(t *testing.T) (launchservice.Layout, []byte) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "tun-proxy-supervisor-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	layout := launchservice.Layout{
		Label:         "com.example.tun-proxy",
		Binary:        filepath.Join(root, "bin", "tun-proxy"),
		Config:        filepath.Join(root, "etc", "config.yaml"),
		Plist:         filepath.Join(root, "LaunchDaemons", "com.example.tun-proxy.plist"),
		LogDirectory:  filepath.Join(root, "logs"),
		StandardOut:   filepath.Join(root, "logs", "stdout.log"),
		StandardErr:   filepath.Join(root, "logs", "stderr.log"),
		RuntimeDir:    filepath.Join(root, "run"),
		WorkerUser:    "_tun-proxy",
		WorkerGroup:   "_tun-proxy",
		WorkerDir:     filepath.Join(root, "run", "worker"),
		StatusSocket:  filepath.Join(root, "run", "worker", "status.sock"),
		ControlSocket: filepath.Join(root, "run", "control.sock"),
		DataDir:       filepath.Join(root, "lib"),
		State:         filepath.Join(root, "run", "state.json"),
		Lock:          filepath.Join(root, "run", "lock"),
		OperationLock: filepath.Join(root, "service-operation.lock"),
		FakeIPv4:      filepath.Join(root, "lib", "fake-ip.yaml"),
		FakeIPv6:      filepath.Join(root, "lib", "fake-ipv6.yaml"),
	}
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	replacements := map[string]string{
		"/var/run/tun-proxy/state.json":     layout.State,
		"/var/run/tun-proxy/tun-proxy.lock": layout.Lock,
		"/var/lib/tun-proxy/fake-ip.yaml":   layout.FakeIPv4,
		"/var/lib/tun-proxy/fake-ipv6.yaml": layout.FakeIPv6,
	}
	for old, next := range replacements {
		contents = bytes.ReplaceAll(contents, []byte(old), []byte(next))
	}
	return layout, contents
}

func TestValidateExpectedReloadDigest(t *testing.T) {
	const actual = "sha256:actual"
	if err := validateExpectedReloadDigest(actual, ""); err != nil {
		t.Fatalf("signal reload digest validation = %v", err)
	}
	if err := validateExpectedReloadDigest(actual, actual); err != nil {
		t.Fatalf("matching control digest validation = %v", err)
	}
	if err := validateExpectedReloadDigest(actual, "sha256:expected"); err == nil || !strings.Contains(err.Error(), "does not match requested digest") {
		t.Fatalf("mismatched control digest error = %v", err)
	}
}
