package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/defaultconfig"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
)

func TestValidateConfigFile(t *testing.T) {
	result, err := validateConfigFile("../../configs/config.yaml", false, launchservice.DefaultLayout())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || result.Digest == "" || result.Summary == "" || result.ManagedService {
		t.Fatalf("validation result = %+v", result)
	}
}

func TestRevealConfigInFinder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var executable string
	var arguments []string
	revealed, err := revealConfigInFinder(t.Context(), path,
		func(_ context.Context, name string, args ...string) ([]byte, error) {
			executable = name
			arguments = append([]string(nil), args...)
			return nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if revealed != absolute || executable != "/usr/bin/open" || strings.Join(arguments, " ") != "-R "+absolute {
		t.Fatalf("reveal result path=%q executable=%q args=%q", revealed, executable, arguments)
	}
}

func TestRevealConfigInFinderReportsOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := revealConfigInFinder(t.Context(), path,
		func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Finder unavailable"), errors.New("exit status 1")
		})
	if err == nil || !strings.Contains(err.Error(), "Finder unavailable") {
		t.Fatalf("reveal error = %v", err)
	}
}

func TestRevealConfigInFinderRejectsNonRegularPath(t *testing.T) {
	_, err := revealConfigInFinder(t.Context(), t.TempDir(),
		func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("runner must not be called for a directory")
			return nil, nil
		})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("reveal directory error = %v", err)
	}
}

func TestGenerateDefaultConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	generated, err := generateDefaultConfig(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if generated != path {
		t.Fatalf("generated path = %q, want %q", generated, path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, defaultconfig.Bytes()) {
		t.Fatal("generated config does not match the embedded default")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("generated config mode = %04o, want 0600", got)
	}
	if _, err := validateConfigFile(path, true, launchservice.DefaultLayout()); err != nil {
		t.Fatalf("validate generated config: %v", err)
	}
}

func TestGenerateDefaultConfigRequiresForceToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	sentinel := []byte("keep: existing\n")
	if err := os.WriteFile(path, sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := generateDefaultConfig(path, false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("generate without force error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, sentinel) {
		t.Fatalf("existing config changed without force: %q", contents)
	}
	if _, err := generateDefaultConfig(path, true); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, defaultconfig.Bytes()) {
		t.Fatal("force did not replace config with the embedded default")
	}
}

func TestGenerateDefaultConfigAllowsSymlinkedParentDirectory(t *testing.T) {
	realDirectory := t.TempDir()
	if err := os.Chmod(realDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "config-parent")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "config.yaml")
	if _, err := generateDefaultConfig(path, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(realDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing parent mode = %04o, want 0755", got)
	}
}

func TestValidateConfigFileEnforcesManagedPaths(t *testing.T) {
	result, err := validateConfigFile("../../configs/config.yaml", true, launchservice.DefaultLayout())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || !result.ManagedService {
		t.Fatalf("managed validation result = %+v", result)
	}

	layout := launchservice.DefaultLayout()
	layout.State = "/var/run/tun-proxy/different-state.json"
	if _, err := validateConfigFile("../../configs/config.yaml", true, layout); err == nil || !strings.Contains(err.Error(), "system.state_file") {
		t.Fatalf("managed path validation error = %v", err)
	}
}
