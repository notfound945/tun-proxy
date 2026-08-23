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

	"github.com/hailinpan/tun-proxy/internal/apperror"
	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/control"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
)

const (
	testServiceReloadRequestID = "0123456789abcdef0123456789abcdef"
	testServiceOperationID     = "fedcba9876543210fedcba9876543210"
)

const testServiceReloadDigest = "sha256:09bfcc6a14b83e2192b8673677725c84883ee9cd0c70e45c9ec09daa8f2b2847"

func testServiceReloadRequest() control.ReloadRequest {
	return control.ReloadRequest{
		Version: control.Version, Kind: control.KindReload, RequestID: testServiceReloadRequestID,
		OperationID: testServiceOperationID, ExpectedConfigDigest: testServiceReloadDigest,
	}
}

func TestValidateServiceReloadStatusGuidesStoppedServiceToStart(t *testing.T) {
	err := validateServiceReloadStatus(launchservice.Status{
		Installed: true,
		Loaded:    true,
		Runtime:   launchservice.RuntimeState{Phase: "stopped"},
	})
	if err == nil || !strings.Contains(err.Error(), launchservice.StartCommand) || apperror.CodeOf(err) != apperror.CodeServiceNotRunning {
		t.Fatalf("validateServiceReloadStatus() error = %v code=%s, want command %q", err, apperror.CodeOf(err), launchservice.StartCommand)
	}
}

func TestValidateServiceReloadStatusGuidesMissingServiceToInstall(t *testing.T) {
	err := validateServiceReloadStatus(launchservice.Status{})
	if err == nil || !strings.Contains(err.Error(), launchservice.InstallCommand) || apperror.CodeOf(err) != apperror.CodeServiceNotInstalled {
		t.Fatalf("validateServiceReloadStatus() error = %v code=%s, want command %q", err, apperror.CodeOf(err), launchservice.InstallCommand)
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
	if err == nil || !strings.Contains(err.Error(), launchservice.StartCommand) || apperror.CodeOf(err) != apperror.CodeServiceNotRunning {
		t.Fatalf("validateServiceReloadStatus() error = %v code=%s, want not-running guidance", err, apperror.CodeOf(err))
	}
}

func TestRequestServiceReloadUsesFinalControlResponse(t *testing.T) {
	called := false
	request := testServiceReloadRequest()
	response, err := requestServiceReload(t.Context(), "/tmp/control.sock", 42, request, time.Second,
		func(ctx context.Context, socket string, uid uint32, got control.ReloadRequest) (control.ReloadResponse, error) {
			called = true
			if socket != "/tmp/control.sock" || uid != 42 || got != request {
				t.Fatalf("control request socket=%q uid=%d request=%+v", socket, uid, got)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("control request context has no timeout")
			}
			return control.ReloadResponse{
				Version: control.Version, Kind: control.KindReloadResult, RequestID: got.RequestID,
				Result: control.ResultSucceeded, ConfigDigest: got.ExpectedConfigDigest,
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !called || response.ConfigDigest != testServiceReloadDigest {
		t.Fatalf("called=%t response=%+v", called, response)
	}
}

func TestRequestServiceReloadRetriesTransportAndRunningWithSameID(t *testing.T) {
	request := testServiceReloadRequest()
	attempt := 0
	response, err := requestServiceReload(t.Context(), "/tmp/control.sock", 42, request, time.Second,
		func(_ context.Context, _ string, _ uint32, got control.ReloadRequest) (control.ReloadResponse, error) {
			attempt++
			if got != request {
				t.Fatalf("attempt %d request=%+v, want %+v", attempt, got, request)
			}
			switch attempt {
			case 1:
				return control.ReloadResponse{}, &control.TransportError{Err: errors.New("response lost")}
			case 2:
				return control.ReloadResponse{Version: control.Version, Kind: control.KindReloadResult, RequestID: got.RequestID, Result: control.ResultRunning}, nil
			default:
				return control.ReloadResponse{Version: control.Version, Kind: control.KindReloadResult, RequestID: got.RequestID, Result: control.ResultSucceeded, ConfigDigest: got.ExpectedConfigDigest}, nil
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if attempt != 3 || response.RequestID != request.RequestID {
		t.Fatalf("attempts=%d response=%+v", attempt, response)
	}
}

func TestRequestServiceReloadReturnsWorkerFailureDirectly(t *testing.T) {
	want := errors.New("worker rejected immutable setting")
	_, err := requestServiceReload(t.Context(), "/tmp/control.sock", 0, testServiceReloadRequest(), time.Second,
		func(context.Context, string, uint32, control.ReloadRequest) (control.ReloadResponse, error) {
			info := apperror.InfoOf(apperror.Wrap(apperror.CodeReloadRejected, "service.reload", "worker rejected configuration reload", want))
			return control.ReloadResponse{Result: control.ResultFailed, Error: &info}, want
		})
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("requestServiceReload() error = %v", err)
	}
}

func TestRequestServiceReloadHonorsTimeoutWithoutStatusPolling(t *testing.T) {
	started := time.Now()
	_, err := requestServiceReload(t.Context(), "/tmp/control.sock", 0, testServiceReloadRequest(), 20*time.Millisecond,
		func(ctx context.Context, _ string, _ uint32, _ control.ReloadRequest) (control.ReloadResponse, error) {
			<-ctx.Done()
			return control.ReloadResponse{}, ctx.Err()
		})
	if !errors.Is(err, context.DeadlineExceeded) || apperror.CodeOf(err) != apperror.CodeReloadTimeout {
		t.Fatalf("requestServiceReload() error = %v code=%s", err, apperror.CodeOf(err))
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout elapsed = %s", elapsed)
	}
}

func TestRequestServiceReloadRejectsUnexpectedDigest(t *testing.T) {
	request := testServiceReloadRequest()
	_, err := requestServiceReload(t.Context(), "/tmp/control.sock", 0, request, time.Second,
		func(context.Context, string, uint32, control.ReloadRequest) (control.ReloadResponse, error) {
			return control.ReloadResponse{Result: control.ResultSucceeded, ConfigDigest: "sha256:other"}, nil
		})
	info := apperror.InfoOf(err)
	if info.Code != apperror.CodeReloadDigestMismatch || info.Details["reload_request_id"] != request.RequestID || info.Details["operation_id"] != request.OperationID {
		t.Fatalf("digest mismatch = %+v", info)
	}
}

func TestRequestServiceReloadRejectsUnexpectedResult(t *testing.T) {
	request := testServiceReloadRequest()
	_, err := requestServiceReload(t.Context(), "/tmp/control.sock", 0, request, time.Second,
		func(context.Context, string, uint32, control.ReloadRequest) (control.ReloadResponse, error) {
			return control.ReloadResponse{Result: "future-result"}, nil
		})
	if got := apperror.CodeOf(err); got != apperror.CodeServiceProtocolTooOld {
		t.Fatalf("unexpected result code = %s, error=%v", got, err)
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
	reload := func(_ context.Context, socket string, uid uint32, request control.ReloadRequest) (control.ReloadResponse, error) {
		called = true
		if socket != "/tmp/control.sock" || uid != 42 || request.ExpectedConfigDigest != wantDigest {
			t.Fatalf("control rollback socket=%q uid=%d request=%+v, want digest=%q", socket, uid, request, wantDigest)
		}
		if request.OperationID != testServiceOperationID || request.RollbackOf != testServiceReloadRequestID || request.RequestID == request.RollbackOf {
			t.Fatalf("rollback request IDs = %+v", request)
		}
		return control.ReloadResponse{Version: control.Version, Kind: control.KindReloadResult, RequestID: request.RequestID, Result: control.ResultSucceeded, ConfigDigest: request.ExpectedConfigDigest}, nil
	}
	update := testServiceConfigRollback{rollback: func() error {
		return os.WriteFile(managedConfig, contents, 0o600)
	}}
	if err := rollbackServiceReloadConfig(t.Context(), manager, update, "/tmp/control.sock", time.Second, testServiceOperationID, testServiceReloadRequestID, reload); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("control rollback was not requested")
	}
}

func TestNewServiceReloadRequestUsesLockedOperationID(t *testing.T) {
	ctx := context.WithValue(t.Context(), serviceOperationIDContextKey{}, testServiceOperationID)
	request, err := newServiceReloadRequest(ctx, testServiceReloadDigest, testServiceReloadRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if request.OperationID != testServiceOperationID || request.RollbackOf != testServiceReloadRequestID || request.RequestID == request.RollbackOf || len(request.RequestID) != 32 {
		t.Fatalf("request = %+v", request)
	}
}

func TestServiceReloadDetailsOmitsEmptyCorrelationFields(t *testing.T) {
	details := serviceReloadDetails(control.ReloadRequest{
		RequestID:   "0123456789abcdef0123456789abcdef",
		OperationID: "fedcba9876543210fedcba9876543210",
	}, "apply")
	if _, ok := details["rollback_of"]; ok {
		t.Fatalf("details unexpectedly contain empty rollback_of: %#v", details)
	}
	if details["reload_request_id"] == "" || details["operation_id"] == "" || details["phase"] != "apply" {
		t.Fatalf("details = %#v", details)
	}
}
