package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
)

func TestCleanupCommandRejectsNonPositiveTimeout(t *testing.T) {
	for _, timeout := range []string{"0s", "-1s"} {
		err := cleanupCommand([]string{"-timeout", timeout})
		if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
			t.Fatalf("cleanupCommand(-timeout %s) error = %v", timeout, err)
		}
	}
}

func TestCleanupFlagsIncludeClearConfiguration(t *testing.T) {
	options := cleanupOptions{}
	flags := newCleanupFlagSet(io.Discard, &options)
	if err := flags.Parse([]string{"-config", "/tmp/config.yaml", "-clear-dns", "-clear-fake-ip"}); err != nil {
		t.Fatal(err)
	}
	if options.configPath != "/tmp/config.yaml" || !options.clearDNS || !options.clearFakeIP {
		t.Fatalf("options = %+v", options)
	}
}

func TestClearFakeIPPersistenceUsesBothConfiguredPools(t *testing.T) {
	directory := t.TempDir()
	ipv4 := filepath.Join(directory, "fake-ip.yaml")
	ipv6 := filepath.Join(directory, "fake-ipv6.yaml")
	for _, path := range []string{ipv4, ipv4 + ".wal", ipv6, ipv6 + ".wal"} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &config.Config{
		FakeIP:   config.FakeIP{PersistenceFile: ipv4},
		FakeIPv6: &config.FakeIPv6{PersistenceFile: ipv6},
	}
	removed, err := clearFakeIPPersistence(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 4 {
		t.Fatalf("removed = %v", removed)
	}
	for _, path := range removed {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("path %q still exists: %v", path, err)
		}
	}
}

func TestManagedCleanupPathsOnlyMatchesDefaultServiceState(t *testing.T) {
	layout := launchservice.DefaultLayout()
	if !managedCleanupPaths(layout.State, layout.Lock) {
		t.Fatal("default managed cleanup paths were not recognized")
	}
	for _, paths := range [][2]string{
		{layout.State + ".other", layout.Lock},
		{layout.State, layout.Lock + ".other"},
	} {
		if managedCleanupPaths(paths[0], paths[1]) {
			t.Fatalf("custom cleanup paths were treated as managed: %q %q", paths[0], paths[1])
		}
	}
}
