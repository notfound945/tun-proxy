//go:build darwin

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

const controlDialTimeout = 2 * time.Second

// Reload asks the root supervisor to reload the exact managed configuration
// identified by expectedDigest and waits for the final worker result.
func Reload(ctx context.Context, path string, expectedServerUID uint32, expectedDigest string) (ReloadResponse, error) {
	request := ReloadRequest{Version: Version, Kind: KindReload, ExpectedConfigDigest: expectedDigest}
	if err := request.validate(); err != nil {
		return ReloadResponse{}, err
	}
	if err := validateSocketPath(path); err != nil {
		return ReloadResponse{}, err
	}
	if _, err := inspectSocket(path, expectedServerUID, true); err != nil {
		return ReloadResponse{}, err
	}
	dialer := net.Dialer{Timeout: controlDialTimeout}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return ReloadResponse{}, fmt.Errorf("connect control socket: %w", err)
	}
	defer connection.Close() //nolint:errcheck // Best-effort client cleanup.
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := writeFrame(connection, request); err != nil {
		return ReloadResponse{}, err
	}
	payload, err := readFrame(connection)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ReloadResponse{}, ctxErr
		}
		return ReloadResponse{}, fmt.Errorf("read control response: %w", err)
	}
	var response ReloadResponse
	if err := decodeStrict(payload, &response); err != nil {
		return ReloadResponse{}, fmt.Errorf("decode control response: %w", err)
	}
	if err := response.validate(); err != nil {
		return ReloadResponse{}, err
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	if response.ConfigDigest != expectedDigest {
		return response, fmt.Errorf("control response config digest %q, want %q", response.ConfigDigest, expectedDigest)
	}
	return response, nil
}
