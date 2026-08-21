package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/launchservice"
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
