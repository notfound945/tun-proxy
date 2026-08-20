package privsep

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"sync"
	"testing"
)

type fakeWorkerRuntime struct {
	mutex       sync.Mutex
	prepared    bool
	committed   bool
	reloads     int
	closed      bool
	prepareErr  error
	commitErr   error
	reloadErr   error
	runtimeDone chan error
}

func newFakeWorkerRuntime() *fakeWorkerRuntime {
	return &fakeWorkerRuntime{runtimeDone: make(chan error, 1)}
}

func (runtime *fakeWorkerRuntime) Prepare(context.Context, Bootstrap) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.prepared = true
	return runtime.prepareErr
}

func (runtime *fakeWorkerRuntime) Commit(context.Context) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.committed = true
	return runtime.commitErr
}

func (runtime *fakeWorkerRuntime) Reload(context.Context, Reload) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.reloads++
	return runtime.reloadErr
}

func (runtime *fakeWorkerRuntime) Close(context.Context) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.closed = true
	return nil
}

func (runtime *fakeWorkerRuntime) Done() <-chan error { return runtime.runtimeDone }

func TestServeWorkerStartupReloadAndShutdown(t *testing.T) {
	supervisorConnection, workerConnection := net.Pipe()
	defer supervisorConnection.Close()
	defer workerConnection.Close()
	runtime := newFakeWorkerRuntime()
	workerDone := make(chan error, 1)
	go func() { workerDone <- ServeWorker(context.Background(), workerConnection, runtime) }()

	codec, _ := NewCodec(supervisorConnection, supervisorConnection)
	handshake, _ := NewSupervisorHandshake(codec)
	bootstrap := validBootstrap()
	if err := handshake.Bootstrap(bootstrap); err != nil {
		t.Fatal(err)
	}
	if _, err := handshake.AwaitPrepared(); err != nil {
		t.Fatal(err)
	}
	if err := handshake.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := handshake.AwaitRunning(); err != nil {
		t.Fatal(err)
	}

	reloadConfig := []byte("version: 2\n")
	reloadDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(reloadConfig))
	if err := codec.Send(KindReload, 7, Reload{Config: reloadConfig, ConfigDigest: reloadDigest}); err != nil {
		t.Fatal(err)
	}
	reloadResult, requestID, err := ReceiveKind[ReloadResult](codec, KindReloadResult)
	if err != nil {
		t.Fatal(err)
	}
	if requestID != 7 || reloadResult.ConfigDigest != reloadDigest || reloadResult.Error != "" {
		t.Fatalf("reload result id=%d payload=%+v", requestID, reloadResult)
	}
	if err := codec.Send(KindShutdown, 8, Shutdown{Reason: "test complete"}); err != nil {
		t.Fatal(err)
	}
	if _, requestID, err := ReceiveKind[Stopped](codec, KindStopped); err != nil || requestID != 8 {
		t.Fatalf("stopped id=%d error=%v", requestID, err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if !runtime.prepared || !runtime.committed || runtime.reloads != 1 || !runtime.closed {
		t.Fatalf("runtime state = %+v", runtime)
	}
}

func TestServeWorkerReportsPrepareFailure(t *testing.T) {
	supervisorConnection, workerConnection := net.Pipe()
	defer supervisorConnection.Close()
	defer workerConnection.Close()
	runtime := newFakeWorkerRuntime()
	runtime.prepareErr = fmt.Errorf("broken graph")
	workerDone := make(chan error, 1)
	go func() { workerDone <- ServeWorker(context.Background(), workerConnection, runtime) }()
	codec, _ := NewCodec(supervisorConnection, supervisorConnection)
	if err := codec.Send(KindBootstrap, 0, validBootstrap()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReceiveKind[Prepared](codec, KindPrepared); err == nil || err.Error() != "prepare worker runtime: broken graph" {
		t.Fatalf("prepare failure = %v", err)
	}
	if err := <-workerDone; err == nil {
		t.Fatal("worker returned success after prepare failure")
	}
}
