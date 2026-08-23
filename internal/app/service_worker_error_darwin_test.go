//go:build darwin

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/apperror"
	"github.com/hailinpan/tun-proxy/internal/privsep"
)

func TestAnnotateWorkerReloadError(t *testing.T) {
	reload := privsep.Reload{
		ReloadRequestID: "0123456789abcdef0123456789abcdef",
		ConfigDigest:    "sha256:09bfcc6a14b83e2192b8673677725c84883ee9cd0c70e45c9ec09daa8f2b2847",
	}
	err := annotateWorkerReloadError(context.Background(), reload, errors.New("data plane failed"))
	info := apperror.InfoOf(err)
	if info.Code != apperror.CodeReloadRejected || info.Details["reload_request_id"] != reload.ReloadRequestID || info.Details["expected_config_digest"] != reload.ConfigDigest || info.Details["phase"] != "worker" {
		t.Fatalf("worker error = %+v", info)
	}
}

func TestAnnotateWorkerReloadDeadline(t *testing.T) {
	reload := privsep.Reload{ReloadRequestID: "0123456789abcdef0123456789abcdef"}
	err := annotateWorkerReloadError(context.Background(), reload, context.DeadlineExceeded)
	if got := apperror.CodeOf(err); got != apperror.CodeReloadTimeout {
		t.Fatalf("deadline code = %s", got)
	}
}

func TestAnnotateWorkerReloadErrorPreservesTypedInternalError(t *testing.T) {
	reload := privsep.Reload{ReloadRequestID: "0123456789abcdef0123456789abcdef"}
	err := annotateWorkerReloadError(
		context.Background(),
		reload,
		apperror.New(apperror.CodeInternalError, "service.reload", "worker state failed"),
	)
	if got := apperror.CodeOf(err); got != apperror.CodeInternalError {
		t.Fatalf("internal error code = %s", got)
	}
}
