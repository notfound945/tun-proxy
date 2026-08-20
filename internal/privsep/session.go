package privsep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const workerCloseTimeout = 10 * time.Second

// WorkerRuntime is the unprivileged application lifecycle driven by the
// private supervisor protocol. Prepare may make inherited listeners live, but
// externally visible host routes and DNS are not installed until Commit.
type WorkerRuntime interface {
	Prepare(context.Context, Bootstrap) error
	Commit(context.Context) error
	Reload(context.Context, Reload) error
	Close(context.Context) error
	Done() <-chan error
}

// ServeWorker runs one worker protocol session. The caller owns the control
// connection and inherited resources and must close them after this returns.
func ServeWorker(ctx context.Context, control io.ReadWriter, runtime WorkerRuntime) (resultErr error) {
	if ctx == nil {
		return errors.New("worker context is required")
	}
	if control == nil {
		return errors.New("worker control connection is required")
	}
	if runtime == nil {
		return errors.New("worker runtime is required")
	}
	codec, err := NewCodec(control, control)
	if err != nil {
		return err
	}
	fatal := func(failure error) error {
		if failure == nil {
			return nil
		}
		_ = codec.Send(KindFatal, 0, Fatal{Error: failure.Error()})
		return failure
	}
	var closeOnce sync.Once
	var closeErr error
	closeRuntime := func() error {
		closeOnce.Do(func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), workerCloseTimeout)
			defer cancel()
			closeErr = runtime.Close(closeCtx)
		})
		return closeErr
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeRuntime())
	}()

	handshake, err := NewWorkerHandshake(codec)
	if err != nil {
		return fatal(err)
	}
	bootstrap, err := handshake.AwaitBootstrap()
	if err != nil {
		return fatal(err)
	}
	if err := runtime.Prepare(ctx, bootstrap); err != nil {
		return fatal(fmt.Errorf("prepare worker runtime: %w", err))
	}
	prepared := Prepared{
		PID: os.Getpid(), UID: NormalizeCredentialID(os.Geteuid()), GID: NormalizeCredentialID(os.Getegid()),
	}
	if err := handshake.Prepared(prepared); err != nil {
		return fatal(err)
	}
	if _, err := handshake.AwaitCommit(); err != nil {
		return fatal(err)
	}
	if err := runtime.Commit(ctx); err != nil {
		return fatal(fmt.Errorf("commit worker runtime: %w", err))
	}
	if err := handshake.Running(); err != nil {
		return fatal(err)
	}

	type received struct {
		message Message
		err     error
	}
	incoming := make(chan received, 1)
	go func() {
		message, receiveErr := codec.Receive()
		incoming <- received{message: message, err: receiveErr}
	}()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case runtimeErr, ok := <-runtime.Done():
			if !ok || runtimeErr == nil {
				runtimeErr = errors.New("worker runtime stopped unexpectedly")
			}
			return fatal(runtimeErr)
		case item := <-incoming:
			if item.err != nil {
				return fmt.Errorf("receive supervisor command: %w", item.err)
			}
			switch item.message.Kind {
			case KindReload:
				if item.message.RequestID == 0 {
					return fatal(errors.New("reload request ID must be non-zero"))
				}
				reload, decodeErr := DecodePayload[Reload](item.message)
				if decodeErr == nil {
					decodeErr = reload.Validate()
				}
				if decodeErr == nil {
					decodeErr = runtime.Reload(ctx, reload)
				}
				result := ReloadResult{ConfigDigest: reload.ConfigDigest}
				if decodeErr != nil {
					result.ConfigDigest = ""
					result.Error = decodeErr.Error()
				}
				if err := codec.Send(KindReloadResult, item.message.RequestID, result); err != nil {
					return err
				}
			case KindShutdown:
				if _, err := DecodePayload[Shutdown](item.message); err != nil {
					return fatal(err)
				}
				closeErr := closeRuntime()
				stopped := Stopped{}
				if closeErr != nil {
					stopped.Error = closeErr.Error()
				}
				if err := codec.Send(KindStopped, item.message.RequestID, stopped); err != nil {
					return errors.Join(closeErr, err)
				}
				return closeErr
			default:
				return fatal(fmt.Errorf("received unsupported worker command %s", item.message.Kind))
			}
			go func() {
				message, receiveErr := codec.Receive()
				incoming <- received{message: message, err: receiveErr}
			}()
		}
	}
}
