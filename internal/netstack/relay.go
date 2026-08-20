package netstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const relayBufferSize = 32 * 1024

var relayBuffers = sync.Pool{New: func() any {
	buffer := make([]byte, relayBufferSize)
	return &buffer
}}

type closeWriter interface {
	CloseWrite() error
}

type copyResult struct {
	direction string
	err       error
}

// Relay copies a TCP byte stream in both directions. A clean EOF half-closes
// only the destination write side so the peer may still send its response.
// Cancellation, an I/O failure, or grace timeout closes both connections and
// waits for both copy goroutines before returning.
func Relay(ctx context.Context, client, upstream net.Conn, graceTimeout time.Duration) error {
	if client == nil || upstream == nil {
		return errors.New("relay requires both client and upstream connections")
	}
	if graceTimeout <= 0 {
		return errors.New("relay grace timeout must be positive")
	}

	results := make(chan copyResult, 2)
	copyOne := func(direction string, dst, src net.Conn) {
		buffer := relayBuffers.Get().(*[]byte)
		_, err := io.CopyBuffer(dst, src, *buffer)
		relayBuffers.Put(buffer)
		if err == nil {
			if writer, ok := dst.(closeWriter); ok {
				err = writer.CloseWrite()
			}
		}
		results <- copyResult{direction: direction, err: err}
	}

	go copyOne("client to upstream", upstream, client)
	go copyOne("upstream to client", client, upstream)

	closeBoth := sync.OnceFunc(func() {
		_ = client.Close()
		_ = upstream.Close()
	})
	defer closeBoth()

	var collected []error
	first := copyResult{}
	select {
	case first = <-results:
		if first.err != nil {
			collected = append(collected, fmt.Errorf("%s: %w", first.direction, first.err))
			closeBoth()
		}
	case <-ctx.Done():
		collected = append(collected, ctx.Err())
		closeBoth()
		first = <-results
		if first.err != nil && !errors.Is(first.err, net.ErrClosed) {
			collected = append(collected, fmt.Errorf("%s: %w", first.direction, first.err))
		}
	}

	timer := time.NewTimer(graceTimeout)
	defer timer.Stop()
	select {
	case second := <-results:
		if second.err != nil && !errors.Is(second.err, net.ErrClosed) {
			collected = append(collected, fmt.Errorf("%s: %w", second.direction, second.err))
		}
	case <-ctx.Done():
		if !errors.Is(errors.Join(collected...), ctx.Err()) {
			collected = append(collected, ctx.Err())
		}
		closeBoth()
		second := <-results
		if second.err != nil && !errors.Is(second.err, net.ErrClosed) {
			collected = append(collected, fmt.Errorf("%s: %w", second.direction, second.err))
		}
	case <-timer.C:
		collected = append(collected, errors.New("relay half-close grace timeout"))
		closeBoth()
		second := <-results
		if second.err != nil && !errors.Is(second.err, net.ErrClosed) {
			collected = append(collected, fmt.Errorf("%s: %w", second.direction, second.err))
		}
	}
	return errors.Join(collected...)
}
