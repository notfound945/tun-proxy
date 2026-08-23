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
	"github.com/hailinpan/tun-proxy/internal/control"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
)

const testServiceReloadDigest = "sha256:09bfcc6a14b83e2192b8673677725c84883ee9cd0c70e45c9ec09daa8f2b2847"

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

func TestValidateServiceReloadStatusRejectsRunningServiceBeforeReady(t *testing.T) {
	err := validateServiceReloadStatus(launchservice.Status{
		Installed: true,
		Loaded:    true,
		Runtime:   launchservice.RuntimeState{Running: true, PID: 42, Phase: "starting"},
	})
	if err == nil || !strings.Contains(err.Error(), launchservice.StartCommand) {
		t.Fatalf("validateServiceReloadStatus() error = %v, want not-running guidance", err)
	}
}

func TestRequestServiceReloadUsesFinalControlResponse(t *testing.T) {
	called := false
	response, err := requestServiceReload(t.Context(), "/tmp/control.sock", 42, testServiceReloadDigest, time.Second,
		func(ctx context.Context, socket string, uid uint32, digest string) (control.ReloadResponse, error) {
			called = true
			if socket != "/tmp/control.sock" || uid != 42 || digest != testServiceReloadDigest {
				t.Fatalf("control request socket=%q uid=%d digest=%q", socket, uid, digest)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("control request context has no timeout")
			}
			return control.ReloadResponse{Version: control.Version, Kind: control.KindReload, ConfigDigest: digest}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !called || response.ConfigDigest != testServiceReloadDigest {
		t.Fatalf("called=%t response=%+v", called, response)
	}
}

func TestRequestServiceReloadReturnsWorkerFailureDirectly(t *testing.T) {
	want := errors.New("worker rejected immutable setting")
	_, err := requestServiceReload(t.Context(), "/tmp/control.sock", 0, testServiceReloadDigest, time.Second,
		func(context.Context, string, uint32, string) (control.ReloadResponse, error) {
			return control.ReloadResponse{Version: control.Version, Kind: control.KindReload, Error: want.Error()}, want
		})
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("requestServiceReload() error = %v", err)
	}
}

func TestRequestServiceReloadHonorsTimeoutWithoutStatusPolling(t *testing.T) {
	started := time.Now()
	_, err := requestServiceReload(t.Context(), "/tmp/control.sock", 0, testServiceReloadDigest, 20*time.Millisecond,
		func(ctx context.Context, _ string, _ uint32, _ string) (control.ReloadResponse, error) {
			<-ctx.Done()
			return control.ReloadResponse{}, ctx.Err()
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("requestServiceReload() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout elapsed = %s", elapsed)
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

type testServiceConfigRollback struct {
	rollback func() error
}

func (rollback testServiceConfigRollback) Rollback() error {
	return rollback.rollback()
}

func TestRollbackServiceReloadUsesRestoredDigestAndControlResponse(t *testing.T) {
	contents, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	managedConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(managedConfig, []byte("new config"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &launchservice.Manager{
		Layout:   launchservice.Layout{Config: managedConfig},
		OwnerUID: 42,
	}
	_, wantDigest, err := config.LoadBytesWithDigest(contents)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	reload := func(_ context.Context, socket string, uid uint32, digest string) (control.ReloadResponse, error) {
		called = true
		if socket != "/tmp/control.sock" || uid != 42 || digest != wantDigest {
			t.Fatalf("control rollback socket=%q uid=%d digest=%q, want digest=%q", socket, uid, digest, wantDigest)
		}
		return control.ReloadResponse{Version: control.Version, Kind: control.KindReload, ConfigDigest: digest}, nil
	}
	update := testServiceConfigRollback{rollback: func() error {
		return os.WriteFile(managedConfig, contents, 0o600)
	}}
	if err := rollbackServiceReloadConfig(manager, update, "/tmp/control.sock", time.Second, reload); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("control rollback was not requested")
	}
}
