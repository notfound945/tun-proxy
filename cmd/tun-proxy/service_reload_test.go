package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
)

func TestValidateServiceReloadStatusGuidesStoppedServiceToStart(t *testing.T) {
	err := validateServiceReloadStatus(launchservice.Status{
		Installed: true,
		Loaded:    true,
		Runtime:   launchservice.RuntimeState{Phase: "stopped"},
	})
	if err == nil || !strings.Contains(err.Error(), launchservice.StartCommand) {
		t.Fatalf("validateServiceReloadStatus() error = %v, want command %q", err, launchservice.StartCommand)
	}
}

func TestValidateServiceReloadStatusGuidesMissingServiceToInstall(t *testing.T) {
	err := validateServiceReloadStatus(launchservice.Status{})
	if err == nil || !strings.Contains(err.Error(), launchservice.InstallCommand) {
		t.Fatalf("validateServiceReloadStatus() error = %v, want command %q", err, launchservice.InstallCommand)
	}
}

func TestValidateServiceReloadStatusAcceptsRunningService(t *testing.T) {
	err := validateServiceReloadStatus(launchservice.Status{
		Installed: true,
		Loaded:    true,
		Runtime:   launchservice.RuntimeState{Running: true, Phase: "running"},
	})
	if err != nil {
		t.Fatalf("validateServiceReloadStatus() error = %v", err)
	}
}

func TestWaitForServiceReloadObservesSuccess(t *testing.T) {
	before := runtimestatus.Snapshot{Reload: runtimestatus.ReloadStats{Successes: 2}}
	after, err := waitForServiceReloadWithQuery(t.Context(), "status.sock", before, time.Second,
		func(context.Context, string) (runtimestatus.Snapshot, error) {
			return runtimestatus.Snapshot{
				ConfigDigest: "sha256:new",
				Reload:       runtimestatus.ReloadStats{Successes: 3},
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if after.ConfigDigest != "sha256:new" || after.Reload.Successes != 3 {
		t.Fatalf("reload result = %+v", after)
	}
}

func TestWaitForServiceReloadReportsRuntimeFailure(t *testing.T) {
	before := runtimestatus.Snapshot{Reload: runtimestatus.ReloadStats{Failures: 4}}
	_, err := waitForServiceReloadWithQuery(t.Context(), "status.sock", before, time.Second,
		func(context.Context, string) (runtimestatus.Snapshot, error) {
			return runtimestatus.Snapshot{Reload: runtimestatus.ReloadStats{
				Failures:  5,
				LastError: "immutable setting changed",
			}}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "immutable setting changed") {
		t.Fatalf("reload failure error = %v", err)
	}
}

func TestWaitForServiceReloadReportsLastQueryError(t *testing.T) {
	queryErr := errors.New("status socket unavailable")
	_, err := waitForServiceReloadWithQuery(t.Context(), "status.sock", runtimestatus.Snapshot{}, time.Millisecond,
		func(context.Context, string) (runtimestatus.Snapshot, error) {
			return runtimestatus.Snapshot{}, queryErr
		})
	if err == nil || !strings.Contains(err.Error(), queryErr.Error()) {
		t.Fatalf("reload timeout error = %v", err)
	}
}

func TestServiceReloadFlagsAcceptConfigPath(t *testing.T) {
	options := serviceReloadOptions{}
	flags := newServiceReloadFlagSet(os.Stderr, &options)
	if err := flags.Parse([]string{"-config", "/tmp/config.yaml", "-timeout", "30s"}); err != nil {
		t.Fatal(err)
	}
	if options.configPath != "/tmp/config.yaml" || options.timeout != 30*time.Second {
		t.Fatalf("reload options = %+v", options)
	}
}

func TestServiceReloadCommandAddsLogsHintOnOperationalFailure(t *testing.T) {
	err := serviceReloadCommand(context.Background(), &launchservice.Manager{}, nil)
	if err == nil {
		t.Fatal("serviceReloadCommand() error = nil")
	}
	if !strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("serviceReloadCommand() error = %q, want command %q", err, serviceLogsHintCommand)
	}
}

func TestServiceReloadCommandRejectsOptionsWithoutLogsHint(t *testing.T) {
	err := serviceReloadCommand(context.Background(), nil, []string{"-timeout", "0s"})
	if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("serviceReloadCommand() error = %v", err)
	}
	if strings.Contains(err.Error(), serviceLogsHintCommand) {
		t.Fatalf("option error unexpectedly included logs hint: %q", err)
	}
}

func TestServiceReloadUserConfigUsesInvokingUserDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")

	options := serviceReloadOptions{}
	flags := newServiceReloadFlagSet(os.Stderr, &options)
	if err := flags.Parse([]string{"-user-config"}); err != nil {
		t.Fatal(err)
	}
	path, err := resolveServiceReloadConfigPath(options)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "tun-proxy", "config.yaml")
	if path != want {
		t.Fatalf("user config path = %q, want %q", path, want)
	}
}

func TestServiceReloadRejectsUserConfigWithExplicitConfig(t *testing.T) {
	_, err := resolveServiceReloadConfigPath(serviceReloadOptions{
		configPath:    "/tmp/config.yaml",
		useUserConfig: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("resolveServiceReloadConfigPath() error = %v", err)
	}
}

func TestLoadValidatedConfigSourceReturnsExactValidatedPayload(t *testing.T) {
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	absolute, gotContents, runtime, digest, err := loadValidatedConfigSource(path)
	if err != nil {
		t.Fatal(err)
	}
	_, wantDigest, err := config.LoadBytesWithDigest(contents)
	if err != nil {
		t.Fatal(err)
	}
	if absolute != path {
		t.Fatalf("absolute path = %q, want %q", absolute, path)
	}
	if !bytes.Equal(gotContents, contents) {
		t.Fatal("validated payload differs from source bytes")
	}
	if runtime == nil || digest != wantDigest {
		t.Fatalf("validated runtime=%v digest=%q, want digest %q", runtime != nil, digest, wantDigest)
	}
}

func TestLoadValidatedConfigSourceRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "config.yaml")
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := loadValidatedConfigSource(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("loadValidatedConfigSource() error = %v", err)
	}
}

func TestLoadValidatedConfigSourceRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, privsep.MaxConfigSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := loadValidatedConfigSource(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("loadValidatedConfigSource() error = %v", err)
	}
}
