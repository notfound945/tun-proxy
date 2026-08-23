//go:build darwin

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/hailinpan/tun-proxy/internal/apperror"
)

const controlDialTimeout = 2 * time.Second

// TransportError indicates that a validated request may have been sent but its
// final response was not observed. Retrying the same RequestID is safe.
type TransportError struct{ Err error }

func (err *TransportError) Error() string {
	if err == nil || err.Err == nil {
		return "control transport failed"
	}
	return err.Err.Error()
}

func (err *TransportError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// IsTransportError reports whether a control operation can be safely retried
// with the exact same request ID.
func IsTransportError(err error) bool {
	var target *TransportError
	return errors.As(err, &target)
}

// Reload submits one idempotent reload request and returns either a running or
// final response. Callers may retry TransportError or ResultRunning responses
// with the exact same request until their own deadline expires.
func Reload(ctx context.Context, path string, expectedServerUID uint32, request ReloadRequest) (ReloadResponse, error) {
	if err := request.validate(); err != nil {
		return ReloadResponse{}, err
	}
	if err := validateSocketPath(path); err != nil {
		return ReloadResponse{}, err
	}
	if _, err := inspectSocket(path, expectedServerUID, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ReloadResponse{}, &TransportError{Err: err}
		}
		return ReloadResponse{}, apperror.Wrap(apperror.CodeUnsafeFile, "service.reload", "supervisor control socket is unsafe", err).WithDetails(map[string]any{"socket_path": path})
	}
	dialer := net.Dialer{Timeout: controlDialTimeout}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ReloadResponse{}, ctxErr
		}
		return ReloadResponse{}, &TransportError{Err: fmt.Errorf("connect control socket: %w", err)}
	}
	defer connection.Close() //nolint:errcheck // Best-effort client cleanup.
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := writeFrame(connection, request); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ReloadResponse{}, ctxErr
		}
		return ReloadResponse{}, &TransportError{Err: err}
	}
	payload, err := readFrame(connection)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ReloadResponse{}, ctxErr
		}
		return ReloadResponse{}, &TransportError{Err: fmt.Errorf("read control response: %w", err)}
	}
	var response ReloadResponse
	if err := decodeStrict(payload, &response); err != nil {
		return ReloadResponse{}, apperror.Wrap(apperror.CodeServiceProtocolTooOld, "service.reload", "supervisor control response is incompatible", err)
	}
	if response.Version != Version {
		return ReloadResponse{}, apperror.Wrap(apperror.CodeServiceProtocolTooOld, "service.reload", "supervisor control protocol version is incompatible", fmt.Errorf("got version %d, want %d", response.Version, Version))
	}
	if err := response.validate(); err != nil {
		return ReloadResponse{}, apperror.Wrap(apperror.CodeServiceProtocolTooOld, "service.reload", "supervisor control response is invalid", err)
	}
	if response.RequestID != request.RequestID {
		return response, apperror.Wrap(apperror.CodeReloadRequestMismatch, "service.reload", "supervisor response request ID does not match the reload request", fmt.Errorf("got %q, want %q", response.RequestID, request.RequestID)).WithDetails(map[string]any{"reload_request_id": request.RequestID})
	}
	if response.Result == ResultFailed {
		return response, apperror.FromInfo(*response.Error)
	}
	if response.Result == ResultSucceeded && response.ConfigDigest != request.ExpectedConfigDigest {
		return response, apperror.Wrap(apperror.CodeReloadDigestMismatch, "service.reload", "supervisor activated an unexpected configuration digest", fmt.Errorf("got %q, want %q", response.ConfigDigest, request.ExpectedConfigDigest)).WithDetails(map[string]any{"expected_config_digest": request.ExpectedConfigDigest, "actual_config_digest": response.ConfigDigest, "reload_request_id": request.RequestID})
	}
	return response, nil
}
