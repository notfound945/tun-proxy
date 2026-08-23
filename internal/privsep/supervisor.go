package privsep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hailinpan/tun-proxy/internal/apperror"
)

// SupervisorSession owns the root side of one private worker protocol
// connection. It is deliberately independent from exec.Cmd so startup and
// failure ordering can be tested without creating privileged processes.
type SupervisorSession struct {
	codec       *Codec
	expectedPID int
	identity    Identity
	incoming    chan receivedMessage
	processExit chan struct{}

	protocolMutex sync.Mutex
	mutex         sync.Mutex
	processErr    error
	requestID     uint64
	bootstrapped  bool
	prepared      bool
	running       bool
	closed        bool
}

type receivedMessage struct {
	message Message
	err     error
}

func NewSupervisorSession(control io.ReadWriter, expectedPID int, identity Identity, processDone <-chan error) (*SupervisorSession, error) {
	if control == nil {
		return nil, errors.New("supervisor control connection is required")
	}
	if expectedPID <= 0 {
		return nil, fmt.Errorf("expected worker PID must be positive, got %d", expectedPID)
	}
	if identity.UID == 0 || identity.GID == 0 {
		return nil, fmt.Errorf("expected worker identity must be non-root, got uid=%d gid=%d", identity.UID, identity.GID)
	}
	codec, err := NewCodec(control, control)
	if err != nil {
		return nil, err
	}
	session := &SupervisorSession{
		codec: codec, expectedPID: expectedPID, identity: identity,
		incoming: make(chan receivedMessage, 1),
	}
	if processDone != nil {
		session.processExit = make(chan struct{})
		go session.observeProcess(processDone)
	}
	go session.receive()
	return session, nil
}

func (session *SupervisorSession) Bootstrap(ctx context.Context, payload Bootstrap) (Prepared, error) {
	if ctx == nil {
		return Prepared{}, errors.New("supervisor bootstrap context is required")
	}
	if err := payload.Validate(); err != nil {
		return Prepared{}, err
	}
	session.protocolMutex.Lock()
	defer session.protocolMutex.Unlock()
	session.mutex.Lock()
	if session.bootstrapped || session.closed {
		session.mutex.Unlock()
		return Prepared{}, errors.New("supervisor session cannot bootstrap in its current state")
	}
	session.bootstrapped = true
	session.mutex.Unlock()
	if err := session.codec.Send(KindBootstrap, 0, payload); err != nil {
		return Prepared{}, fmt.Errorf("send worker bootstrap: %w", err)
	}
	message, err := session.await(ctx, KindPrepared, 0)
	if err != nil {
		return Prepared{}, fmt.Errorf("await worker prepared: %w", err)
	}
	prepared, err := DecodePayload[Prepared](message)
	if err != nil {
		return Prepared{}, err
	}
	if err := prepared.Validate(); err != nil {
		return Prepared{}, err
	}
	if prepared.PID != session.expectedPID || prepared.UID != session.identity.UID || prepared.GID != session.identity.GID {
		return Prepared{}, fmt.Errorf(
			"worker prepared identity pid=%d uid=%d gid=%d, want pid=%d uid=%d gid=%d",
			prepared.PID, prepared.UID, prepared.GID,
			session.expectedPID, session.identity.UID, session.identity.GID,
		)
	}
	session.mutex.Lock()
	session.prepared = true
	session.mutex.Unlock()
	return prepared, nil
}

func (session *SupervisorSession) Commit(ctx context.Context, configDigest string) error {
	if ctx == nil {
		return errors.New("supervisor commit context is required")
	}
	session.protocolMutex.Lock()
	defer session.protocolMutex.Unlock()
	session.mutex.Lock()
	if !session.prepared || session.running || session.closed {
		session.mutex.Unlock()
		return errors.New("supervisor session cannot commit in its current state")
	}
	session.mutex.Unlock()
	if err := validateDigest(configDigest); err != nil {
		return err
	}
	if err := session.codec.Send(KindCommit, 0, Commit{ConfigDigest: configDigest}); err != nil {
		return fmt.Errorf("send worker commit: %w", err)
	}
	message, err := session.await(ctx, KindRunning, 0)
	if err != nil {
		return fmt.Errorf("await worker running: %w", err)
	}
	running, err := DecodePayload[Running](message)
	if err != nil {
		return err
	}
	if running.ConfigDigest != configDigest {
		return fmt.Errorf("worker running digest=%q, want %q", running.ConfigDigest, configDigest)
	}
	session.mutex.Lock()
	session.running = true
	session.mutex.Unlock()
	return nil
}

func (session *SupervisorSession) Reload(ctx context.Context, payload Reload) error {
	if ctx == nil {
		return errors.New("supervisor reload context is required")
	}
	if err := payload.Validate(); err != nil {
		return err
	}
	session.protocolMutex.Lock()
	defer session.protocolMutex.Unlock()
	requestID, err := session.nextRequest()
	if err != nil {
		return err
	}
	if err := session.codec.Send(KindReload, requestID, payload); err != nil {
		return fmt.Errorf("send worker reload: %w", err)
	}
	message, err := session.await(ctx, KindReloadResult, requestID)
	if err != nil {
		return fmt.Errorf("await worker reload result: %w", err)
	}
	result, err := DecodePayload[ReloadResult](message)
	if err != nil {
		return err
	}
	if err := result.Validate(); err != nil {
		return err
	}
	if result.ReloadRequestID != payload.ReloadRequestID {
		return apperror.Wrap(apperror.CodeReloadRequestMismatch, "service.reload", "worker reload request ID does not match the reload request", fmt.Errorf("got %q, want %q", result.ReloadRequestID, payload.ReloadRequestID)).WithDetails(map[string]any{"reload_request_id": payload.ReloadRequestID})
	}
	if result.Error != nil {
		return apperror.FromInfo(*result.Error)
	}
	if result.ConfigDigest != payload.ConfigDigest {
		return apperror.Wrap(apperror.CodeReloadDigestMismatch, "service.reload", "worker activated an unexpected configuration digest", fmt.Errorf("got %q, want %q", result.ConfigDigest, payload.ConfigDigest)).WithDetails(map[string]any{"expected_config_digest": payload.ConfigDigest, "actual_config_digest": result.ConfigDigest, "reload_request_id": payload.ReloadRequestID})
	}
	return nil
}

func (session *SupervisorSession) Shutdown(ctx context.Context, reason string) error {
	if ctx == nil {
		return errors.New("supervisor shutdown context is required")
	}
	session.protocolMutex.Lock()
	defer session.protocolMutex.Unlock()
	session.mutex.Lock()
	if session.closed {
		session.mutex.Unlock()
		return nil
	}
	session.mutex.Unlock()
	requestID, err := session.nextRequest()
	if err != nil {
		return err
	}
	if err := session.codec.Send(KindShutdown, requestID, Shutdown{Reason: reason}); err != nil {
		return fmt.Errorf("send worker shutdown: %w", err)
	}
	message, exitedCleanly, err := session.awaitShutdown(ctx, requestID)
	if err != nil {
		return fmt.Errorf("await worker stopped: %w", err)
	}
	session.mutex.Lock()
	session.closed = true
	session.running = false
	session.mutex.Unlock()
	if exitedCleanly {
		return nil
	}
	stopped, err := DecodePayload[Stopped](message)
	if err != nil {
		return err
	}
	if stopped.Error != "" {
		return errors.New(stopped.Error)
	}
	return nil
}

// WaitProcess waits for the spawned worker to exit. A nil process channel is
// allowed for protocol-only tests and is treated as already complete.
func (session *SupervisorSession) WaitProcess(ctx context.Context) error {
	if ctx == nil {
		return errors.New("supervisor wait context is required")
	}
	if session.processExit == nil {
		return nil
	}
	select {
	case <-session.processExit:
		session.mutex.Lock()
		err := session.processErr
		session.mutex.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ProcessExited is closed when the spawned worker exits. It is nil for
// protocol-only sessions that were constructed without a process channel.
// Multiple observers may wait on it without racing to consume the exit result.
func (session *SupervisorSession) ProcessExited() <-chan struct{} {
	return session.processExit
}

func (session *SupervisorSession) nextRequest() (uint64, error) {
	session.mutex.Lock()
	defer session.mutex.Unlock()
	if !session.running || session.closed {
		return 0, errors.New("supervisor session is not running")
	}
	session.requestID++
	if session.requestID == 0 {
		return 0, errors.New("supervisor request ID exhausted")
	}
	return session.requestID, nil
}

func (session *SupervisorSession) await(ctx context.Context, want Kind, requestID uint64) (Message, error) {
	for {
		select {
		case item := <-session.incoming:
			if item.err != nil {
				return Message{}, item.err
			}
			if item.message.Kind == KindFatal {
				fatal, err := DecodePayload[Fatal](item.message)
				if err != nil {
					return Message{}, err
				}
				if fatal.Error == "" {
					fatal.Error = "worker reported an unspecified fatal error"
				}
				return Message{}, errors.New(fatal.Error)
			}
			if staleReloadResult(item.message, requestID) {
				continue
			}
			if item.message.Kind != want {
				return Message{}, fmt.Errorf("received %s message, want %s", item.message.Kind, want)
			}
			if item.message.RequestID != requestID {
				return Message{}, fmt.Errorf("received %s request ID=%d, want %d", item.message.Kind, item.message.RequestID, requestID)
			}
			return item.message, nil
		case <-session.processExit:
			session.mutex.Lock()
			err := session.processErr
			session.mutex.Unlock()
			if err == nil {
				return Message{}, errors.New("worker exited unexpectedly")
			}
			return Message{}, fmt.Errorf("worker exited unexpectedly: %w", err)
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
}

// awaitShutdown treats a clean worker exit as an implicit acknowledgement once
// the shutdown request has been sent. The worker sends Stopped immediately
// before returning, so the process notification and protocol response can
// become ready concurrently. A non-zero or signaled exit remains a failure.
func (session *SupervisorSession) awaitShutdown(ctx context.Context, requestID uint64) (Message, bool, error) {
	for {
		select {
		case item := <-session.incoming:
			if item.err != nil {
				return Message{}, false, item.err
			}
			if item.message.Kind == KindFatal {
				fatal, err := DecodePayload[Fatal](item.message)
				if err != nil {
					return Message{}, false, err
				}
				if fatal.Error == "" {
					fatal.Error = "worker reported an unspecified fatal error"
				}
				return Message{}, false, errors.New(fatal.Error)
			}
			if staleReloadResult(item.message, requestID) {
				continue
			}
			if item.message.Kind != KindStopped {
				return Message{}, false, fmt.Errorf("received %s message, want %s", item.message.Kind, KindStopped)
			}
			if item.message.RequestID != requestID {
				return Message{}, false, fmt.Errorf("received %s request ID=%d, want %d", item.message.Kind, item.message.RequestID, requestID)
			}
			return item.message, false, nil
		case <-session.processExit:
			session.mutex.Lock()
			err := session.processErr
			session.mutex.Unlock()
			if err != nil {
				return Message{}, false, fmt.Errorf("worker exited during shutdown: %w", err)
			}
			return Message{}, true, nil
		case <-ctx.Done():
			return Message{}, false, ctx.Err()
		}
	}
}

func staleReloadResult(message Message, requestID uint64) bool {
	return message.Kind == KindReloadResult && message.RequestID != 0 && message.RequestID < requestID
}

func (session *SupervisorSession) observeProcess(processDone <-chan error) {
	err, ok := <-processDone
	if !ok {
		err = nil
	}
	session.mutex.Lock()
	session.processErr = err
	session.mutex.Unlock()
	close(session.processExit)
}

func (session *SupervisorSession) receive() {
	for {
		message, err := session.codec.Receive()
		if errors.Is(err, io.EOF) {
			err = errors.New("worker control channel closed")
		}
		session.incoming <- receivedMessage{message: message, err: err}
		if err != nil {
			return
		}
	}
}
