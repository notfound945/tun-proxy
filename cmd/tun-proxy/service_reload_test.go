package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
)

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
