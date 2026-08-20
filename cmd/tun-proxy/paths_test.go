package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultUserConfigPathUsesHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")

	want := filepath.Join(home, ".config", "tun-proxy", "config.yaml")
	if got := defaultUserConfigPath(); got != want {
		t.Fatalf("defaultUserConfigPath() = %q, want %q", got, want)
	}
}
