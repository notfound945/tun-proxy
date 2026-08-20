//go:build darwin

package app

import (
	"context"
	"errors"
	"testing"
)

func TestServiceWorkerCommitRejectsPendingComponentFailure(t *testing.T) {
	failure := errors.New("component failed during prepare")
	worker := &ServiceWorker{prepared: true, done: make(chan error, 1)}
	worker.done <- failure

	if err := worker.Commit(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("Commit() error = %v, want %v", err, failure)
	}
	if worker.committed {
		t.Fatal("worker was committed after a component failure")
	}
}
