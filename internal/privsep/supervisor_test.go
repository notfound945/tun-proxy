package privsep

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSupervisorSessionProtocolLifecycle(t *testing.T) {
	supervisorConnection, workerConnection := net.Pipe()
	defer supervisorConnection.Close() //nolint:errcheck // Best-effort cleanup.
	defer workerConnection.Close()     //nolint:errcheck // Best-effort cleanup.
	identity := Identity{UID: 501, GID: 20}
	processDone := make(chan error, 1)
	session, err := NewSupervisorSession(supervisorConnection, 99, identity, processDone)
	if err != nil {
		t.Fatal(err)
	}

	peerDone := make(chan error, 1)
	go func() {
		codec, _ := NewCodec(workerConnection, workerConnection)
		bootstrap, _, err := ReceiveKind[Bootstrap](codec, KindBootstrap)
		if err != nil {
			peerDone <- err
			return
		}
		if err := codec.Send(KindPrepared, 0, Prepared{PID: 99, UID: 501, GID: 20}); err != nil {
			peerDone <- err
			return
		}
		commit, _, err := ReceiveKind[Commit](codec, KindCommit)
		if err != nil {
			peerDone <- err
			return
		}
		if commit.ConfigDigest != bootstrap.ConfigDigest {
			peerDone <- errors.New("commit digest mismatch")
			return
		}
		if err := codec.Send(KindRunning, 0, Running{ConfigDigest: commit.ConfigDigest}); err != nil {
			peerDone <- err
			return
		}
		reload, requestID, err := ReceiveKind[Reload](codec, KindReload)
		if err != nil {
			peerDone <- err
			return
		}
		if err := codec.Send(KindReloadResult, requestID, ReloadResult{ConfigDigest: reload.ConfigDigest}); err != nil {
			peerDone <- err
			return
		}
		_, requestID, err = ReceiveKind[Shutdown](codec, KindShutdown)
		if err != nil {
			peerDone <- err
			return
		}
		if err := codec.Send(KindStopped, requestID, Stopped{}); err != nil {
			peerDone <- err
			return
		}
		peerDone <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Bootstrap(ctx, validBootstrap()); err != nil {
		t.Fatal(err)
	}
	if err := session.Commit(ctx, testDigest); err != nil {
		t.Fatal(err)
	}
	contents := []byte("version: 2\n")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
	if err := session.Reload(ctx, Reload{Config: contents, ConfigDigest: digest}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shutdown(ctx, "test complete"); err != nil {
		t.Fatal(err)
	}
	processDone <- nil
	if err := session.WaitProcess(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorSessionRejectsPreparedIdentityMismatch(t *testing.T) {
	supervisorConnection, workerConnection := net.Pipe()
	defer supervisorConnection.Close() //nolint:errcheck // Best-effort cleanup.
	defer workerConnection.Close()     //nolint:errcheck // Best-effort cleanup.
	session, err := NewSupervisorSession(supervisorConnection, 99, Identity{UID: 501, GID: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		codec, _ := NewCodec(workerConnection, workerConnection)
		_, _, _ = ReceiveKind[Bootstrap](codec, KindBootstrap)
		_ = codec.Send(KindPrepared, 0, Prepared{PID: 100, UID: 501, GID: 20})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Bootstrap(ctx, validBootstrap()); err == nil || !strings.Contains(err.Error(), "want pid=99") {
		t.Fatalf("Bootstrap() error = %v", err)
	}
}

func TestSupervisorSessionReportsWorkerExitAndReloadFailure(t *testing.T) {
	t.Run("worker exit", func(t *testing.T) {
		supervisorConnection, workerConnection := net.Pipe()
		defer supervisorConnection.Close() //nolint:errcheck // Best-effort cleanup.
		defer workerConnection.Close()     //nolint:errcheck // Best-effort cleanup.
		processDone := make(chan error, 1)
		session, err := NewSupervisorSession(supervisorConnection, 99, Identity{UID: 501, GID: 20}, processDone)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			codec, _ := NewCodec(workerConnection, workerConnection)
			_, _, _ = ReceiveKind[Bootstrap](codec, KindBootstrap)
		}()
		processDone <- errors.New("signal: killed")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := session.Bootstrap(ctx, validBootstrap()); err == nil || !strings.Contains(err.Error(), "signal: killed") {
			t.Fatalf("Bootstrap() error = %v", err)
		}
	})

	t.Run("reload rejected", func(t *testing.T) {
		supervisorConnection, workerConnection := net.Pipe()
		defer supervisorConnection.Close() //nolint:errcheck // Best-effort cleanup.
		defer workerConnection.Close()     //nolint:errcheck // Best-effort cleanup.
		session, err := NewSupervisorSession(supervisorConnection, 99, Identity{UID: 501, GID: 20}, nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			codec, _ := NewCodec(workerConnection, workerConnection)
			bootstrap, _, _ := ReceiveKind[Bootstrap](codec, KindBootstrap)
			_ = codec.Send(KindPrepared, 0, Prepared{PID: 99, UID: 501, GID: 20})
			_, _, _ = ReceiveKind[Commit](codec, KindCommit)
			_ = codec.Send(KindRunning, 0, Running{ConfigDigest: bootstrap.ConfigDigest})
			_, requestID, _ := ReceiveKind[Reload](codec, KindReload)
			_ = codec.Send(KindReloadResult, requestID, ReloadResult{Error: "invalid reload"})
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := session.Bootstrap(ctx, validBootstrap()); err != nil {
			t.Fatal(err)
		}
		if err := session.Commit(ctx, testDigest); err != nil {
			t.Fatal(err)
		}
		contents := []byte("version: 2\n")
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
		if err := session.Reload(ctx, Reload{Config: contents, ConfigDigest: digest}); err == nil || err.Error() != "invalid reload" {
			t.Fatalf("Reload() error = %v", err)
		}
	})
}

func TestSupervisorSessionRecoversAfterTimedOutReload(t *testing.T) {
	supervisorConnection, workerConnection := net.Pipe()
	defer supervisorConnection.Close() //nolint:errcheck // Best-effort cleanup.
	defer workerConnection.Close()     //nolint:errcheck // Best-effort cleanup.
	session, err := NewSupervisorSession(supervisorConnection, 99, Identity{UID: 501, GID: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}

	firstReceived := make(chan struct{})
	releaseFirst := make(chan struct{})
	peerDone := make(chan error, 1)
	go func() {
		codec, codecErr := NewCodec(workerConnection, workerConnection)
		if codecErr != nil {
			peerDone <- codecErr
			return
		}
		bootstrap, _, receiveErr := ReceiveKind[Bootstrap](codec, KindBootstrap)
		if receiveErr != nil {
			peerDone <- receiveErr
			return
		}
		if sendErr := codec.Send(KindPrepared, 0, Prepared{PID: 99, UID: 501, GID: 20}); sendErr != nil {
			peerDone <- sendErr
			return
		}
		if _, _, receiveErr = ReceiveKind[Commit](codec, KindCommit); receiveErr != nil {
			peerDone <- receiveErr
			return
		}
		if sendErr := codec.Send(KindRunning, 0, Running{ConfigDigest: bootstrap.ConfigDigest}); sendErr != nil {
			peerDone <- sendErr
			return
		}
		first, firstID, receiveErr := ReceiveKind[Reload](codec, KindReload)
		if receiveErr != nil {
			peerDone <- receiveErr
			return
		}
		close(firstReceived)
		<-releaseFirst
		if sendErr := codec.Send(KindReloadResult, firstID, ReloadResult{ConfigDigest: first.ConfigDigest}); sendErr != nil {
			peerDone <- sendErr
			return
		}
		second, secondID, receiveErr := ReceiveKind[Reload](codec, KindReload)
		if receiveErr != nil {
			peerDone <- receiveErr
			return
		}
		if sendErr := codec.Send(KindReloadResult, secondID, ReloadResult{ConfigDigest: second.ConfigDigest}); sendErr != nil {
			peerDone <- sendErr
			return
		}
		_, shutdownID, receiveErr := ReceiveKind[Shutdown](codec, KindShutdown)
		if receiveErr != nil {
			peerDone <- receiveErr
			return
		}
		peerDone <- codec.Send(KindStopped, shutdownID, Stopped{})
	}()

	lifecycleCtx, cancelLifecycle := context.WithTimeout(context.Background(), time.Second)
	defer cancelLifecycle()
	if _, err := session.Bootstrap(lifecycleCtx, validBootstrap()); err != nil {
		t.Fatal(err)
	}
	if err := session.Commit(lifecycleCtx, testDigest); err != nil {
		t.Fatal(err)
	}
	contents := []byte("version: 2\n")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelFirst()
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- session.Reload(firstCtx, Reload{Config: contents, ConfigDigest: digest})
	}()
	select {
	case <-firstReceived:
	case <-lifecycleCtx.Done():
		t.Fatal(lifecycleCtx.Err())
	}
	if err := <-firstResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Reload error = %v, want deadline exceeded", err)
	}
	close(releaseFirst)

	if err := session.Reload(lifecycleCtx, Reload{Config: contents, ConfigDigest: digest}); err != nil {
		t.Fatalf("second Reload error = %v", err)
	}
	if err := session.Shutdown(lifecycleCtx, "test complete"); err != nil {
		t.Fatalf("Shutdown error = %v", err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorSessionProcessExitIsRepeatable(t *testing.T) {
	supervisorConnection, workerConnection := net.Pipe()
	defer supervisorConnection.Close() //nolint:errcheck // Best-effort cleanup.
	defer workerConnection.Close()     //nolint:errcheck // Best-effort cleanup.
	processDone := make(chan error, 1)
	session, err := NewSupervisorSession(supervisorConnection, 99, Identity{UID: 501, GID: 20}, processDone)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("worker exit sentinel")
	processDone <- sentinel
	close(processDone)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-session.ProcessExited():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := session.WaitProcess(ctx); !errors.Is(err, sentinel) {
			t.Fatalf("WaitProcess() attempt %d error = %v", attempt, err)
		}
	}
}

func TestSupervisorSessionShutdownAcceptsConcurrentCleanExit(t *testing.T) {
	for attempt := 1; attempt <= 100; attempt++ {
		supervisorConnection, workerConnection := net.Pipe()
		processDone := make(chan error, 1)
		session, err := NewSupervisorSession(supervisorConnection, 99, Identity{UID: 501, GID: 20}, processDone)
		if err != nil {
			t.Fatal(err)
		}

		peerDone := make(chan error, 1)
		go func() {
			codec, codecErr := NewCodec(workerConnection, workerConnection)
			if codecErr != nil {
				peerDone <- codecErr
				return
			}
			bootstrap, _, receiveErr := ReceiveKind[Bootstrap](codec, KindBootstrap)
			if receiveErr != nil {
				peerDone <- receiveErr
				return
			}
			if sendErr := codec.Send(KindPrepared, 0, Prepared{PID: 99, UID: 501, GID: 20}); sendErr != nil {
				peerDone <- sendErr
				return
			}
			if _, _, receiveErr = ReceiveKind[Commit](codec, KindCommit); receiveErr != nil {
				peerDone <- receiveErr
				return
			}
			if sendErr := codec.Send(KindRunning, 0, Running{ConfigDigest: bootstrap.ConfigDigest}); sendErr != nil {
				peerDone <- sendErr
				return
			}
			_, requestID, receiveErr := ReceiveKind[Shutdown](codec, KindShutdown)
			if receiveErr != nil {
				peerDone <- receiveErr
				return
			}
			if sendErr := codec.Send(KindStopped, requestID, Stopped{}); sendErr != nil {
				peerDone <- sendErr
				return
			}
			processDone <- nil
			peerDone <- nil
		}()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if _, err := session.Bootstrap(ctx, validBootstrap()); err != nil {
			cancel()
			t.Fatalf("attempt %d Bootstrap() error = %v", attempt, err)
		}
		if err := session.Commit(ctx, testDigest); err != nil {
			cancel()
			t.Fatalf("attempt %d Commit() error = %v", attempt, err)
		}
		if err := session.Shutdown(ctx, "test complete"); err != nil {
			cancel()
			t.Fatalf("attempt %d Shutdown() error = %v", attempt, err)
		}
		if err := session.WaitProcess(ctx); err != nil {
			cancel()
			t.Fatalf("attempt %d WaitProcess() error = %v", attempt, err)
		}
		if err := <-peerDone; err != nil {
			cancel()
			t.Fatalf("attempt %d peer error = %v", attempt, err)
		}
		cancel()
		_ = supervisorConnection.Close()
		_ = workerConnection.Close()
	}
}
