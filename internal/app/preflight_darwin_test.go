//go:build darwin

package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestCheckPathsHonorsManagedPersistenceOwner(t *testing.T) {
	runtime, err := config.LoadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "tun-proxy-preflight-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	runtimeDir := filepath.Join(root, "run")
	dataDir := filepath.Join(root, "data")
	for _, directory := range []string{runtimeDir, dataDir} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtime.System.StateFile = filepath.Join(runtimeDir, "state.json")
	runtime.System.LockFile = filepath.Join(runtimeDir, "lock")
	runtime.FakeIP.PersistenceFile = filepath.Join(dataDir, "fake-ip.yaml")
	runtime.FakeIPv6.PersistenceFile = filepath.Join(dataDir, "fake-ipv6.yaml")
	for _, path := range []string{runtime.FakeIP.PersistenceFile, runtime.FakeIPv6.PersistenceFile} {
		if err := os.WriteFile(path, []byte("mappings: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	uid := os.Geteuid()
	if err := checkPaths(runtime, pathOwnership{systemUID: uid, persistenceUID: uid}); err != nil {
		t.Fatalf("split path validation rejected expected ownership: %v", err)
	}

	err = checkPaths(runtime, pathOwnership{systemUID: uid, persistenceUID: uid + 1})
	if err == nil {
		t.Fatal("worker persistence owned by the wrong UID was accepted")
	}
	message := err.Error()
	if !strings.Contains(message, "fake_ip.persistence_file") || !strings.Contains(message, "fake_ipv6.persistence_file") {
		t.Fatalf("persistence ownership error = %v", err)
	}
	if strings.Contains(message, "system.state_file") || strings.Contains(message, "system.lock_file") {
		t.Fatalf("worker ownership contaminated root path validation: %v", err)
	}
}

func TestPreflightManagedRejectsRootWorker(t *testing.T) {
	err := PreflightManaged(context.Background(), &config.Config{}, 0)
	if err == nil || !strings.Contains(err.Error(), "must be non-root") {
		t.Fatalf("PreflightManaged() error = %v", err)
	}
}
