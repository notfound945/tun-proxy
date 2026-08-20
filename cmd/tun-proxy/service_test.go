package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/privsep"
)

func TestServiceRunRequiresInstalledConfigPath(t *testing.T) {
	err := serviceRunCommand([]string{"-config", "/tmp/config.yaml"})
	if err == nil || !strings.Contains(err.Error(), "managed service requires") {
		t.Fatalf("serviceRunCommand() error = %v", err)
	}
}

func TestAbsoluteRegularSourceRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	real := filepath.Join(directory, "real")
	if err := os.WriteFile(real, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := absoluteRegularSource(link); err == nil {
		t.Fatal("symlink source was accepted")
	}
}

func TestValidateConfigSourceAcceptsCanonicalConfig(t *testing.T) {
	path, err := validateConfigSource("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("validated path is not absolute: %q", path)
	}
}

func TestServiceCommandRejectsUnknownSubcommand(t *testing.T) {
	err := serviceCommand([]string{"unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown service command") {
		t.Fatalf("serviceCommand() error = %v", err)
	}
}

func TestServiceWorkerInvocationRejectsArgumentsBeforeIdentityLookup(t *testing.T) {
	tests := [][]string{
		{"-config", "/tmp/config.yaml"},
		{"-uid", "501"},
		{"-gid", "20"},
		{"/tmp/control.sock"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			resolved := false
			err := validateServiceWorkerInvocation(
				args,
				func() (privsep.Identity, error) {
					resolved = true
					return privsep.Identity{}, nil
				},
				func(privsep.Identity) error { return nil },
			)
			if err == nil || !strings.Contains(err.Error(), "does not accept arguments") {
				t.Fatalf("validateServiceWorkerInvocation(%q) error = %v", args, err)
			}
			if resolved {
				t.Fatal("worker identity was resolved before rejecting arguments")
			}
		})
	}
}

func TestServiceWorkerInvocationRejectsWrongCurrentIdentity(t *testing.T) {
	want := errors.New("worker credential mismatch")
	identity := privsep.Identity{
		User: privsep.ProductionUser, Group: privsep.ProductionGroup,
		UID: 499, GID: 499, Home: privsep.ProductionHome,
	}
	err := validateServiceWorkerInvocation(
		nil,
		func() (privsep.Identity, error) { return identity, nil },
		func(got privsep.Identity) error {
			if got != identity {
				t.Fatalf("validated identity = %+v, want %+v", got, identity)
			}
			return want
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("validateServiceWorkerInvocation() error = %v, want %v", err, want)
	}
}
