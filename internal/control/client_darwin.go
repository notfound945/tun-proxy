//go:build darwin

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
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
		return ReloadResponse{}, err
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
		return ReloadResponse{}, fmt.Errorf("decode control response: %w", err)
	}
	if err := response.validate(); err != nil {
		return ReloadResponse{}, err
	}
	if response.RequestID != request.RequestID {
		return response, fmt.Errorf("control response request ID %q, want %q", response.RequestID, request.RequestID)
	}
	if response.Result == ResultFailed {
		return response, errors.New(response.Error)
	}
	if response.Result == ResultSucceeded && response.ConfigDigest != request.ExpectedConfigDigest {
		return response, fmt.Errorf("control response config digest %q, want %q", response.ConfigDigest, request.ExpectedConfigDigest)
	}
	return response, nil
}
