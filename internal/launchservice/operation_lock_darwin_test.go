//go:build darwin

package launchservice

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const operationLockHelperEnv = "TUN_PROXY_OPERATION_LOCK_HELPER"

func TestOperationLockRejectsCompetingProcessAndRecoversAfterCrash(t *testing.T) {
	root := shortOperationLockRoot(t)
	layout := operationLockTestLayout(root)
	ready := filepath.Join(root, "helper.ready")
	var stderr bytes.Buffer
	command := exec.Command(os.Args[0], "-test.run=^TestOperationLockHelperProcess$")
	command.Env = append(os.Environ(),
		operationLockHelperEnv+"=1",
		"TUN_PROXY_OPERATION_LOCK_ROOT="+root,
		"TUN_PROXY_OPERATION_LOCK_READY="+ready,
	)
	command.Stdin = strings.NewReader("")
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	processDone := false
	defer func() {
		if !processDone {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	waitForOperationLockHelper(t, ready, &stderr)

	manager := operationLockTestManager(layout)
	_, err := manager.BeginOperation(context.Background(), OperationSpec{Kind: OperationReload})
	if !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("BeginOperation() error = %v, want ErrOperationBusy", err)
	}
	var busy *OperationBusyError
	if !errors.As(err, &busy) || busy.Holder == nil {
		t.Fatalf("BeginOperation() error = %#v, want holder metadata", err)
	}
	if busy.Holder.Kind != OperationUpgrade || busy.Holder.PID != command.Process.Pid {
		t.Fatalf("holder = %+v, want operation=%s pid=%d", busy.Holder, OperationUpgrade, command.Process.Pid)
	}

	// Metadata is diagnostic only. Damaging it must not make a held flock look
	// available to a competing process.
	if err := os.WriteFile(layout.OperationLock, []byte("{damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = manager.BeginOperation(context.Background(), OperationSpec{Kind: OperationStop})
	if !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("BeginOperation() with damaged metadata error = %v, want ErrOperationBusy", err)
	}
	busy = nil
	if !errors.As(err, &busy) || busy.Holder != nil {
		t.Fatalf("damaged metadata busy error = %#v, want nil holder", err)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed operation-lock helper exited successfully")
	}
	processDone = true

	guard, err := manager.BeginOperation(context.Background(), OperationSpec{Kind: OperationCleanup})
	if err != nil {
		t.Fatalf("BeginOperation() after holder crash: %v; helper stderr: %s", err, stderr.String())
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLockHelperProcess(t *testing.T) {
	if os.Getenv(operationLockHelperEnv) != "1" {
		t.Skip("helper subprocess only")
	}
	root := os.Getenv("TUN_PROXY_OPERATION_LOCK_ROOT")
	ready := os.Getenv("TUN_PROXY_OPERATION_LOCK_READY")
	manager := operationLockTestManager(operationLockTestLayout(root))
	guard, err := manager.BeginOperation(context.Background(), OperationSpec{Kind: OperationUpgrade})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close() //nolint:errcheck // The parent intentionally kills this helper.
	if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestOperationLockWritesMetadataAndLeavesSecureReusableFile(t *testing.T) {
	layout := operationLockTestLayout(shortOperationLockRoot(t))
	manager := operationLockTestManager(layout)
	guard, err := manager.BeginOperation(context.Background(), OperationSpec{
		Kind:                 OperationReload,
		ExpectedConfigDigest: "sha256:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(guard.Metadata.ID) != 32 || guard.Metadata.PID != os.Getpid() || guard.Metadata.Kind != OperationReload {
		t.Fatalf("metadata = %+v", guard.Metadata)
	}
	if guard.Metadata.ExpectedConfigDigest != "sha256:test" || guard.Metadata.StartedAt.Location() != time.UTC {
		t.Fatalf("metadata = %+v", guard.Metadata)
	}
	contents, err := os.ReadFile(layout.OperationLock)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(contents, []byte(guard.Metadata.ID)) {
		t.Fatalf("operation metadata %q does not contain ID %q", contents, guard.Metadata.ID)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	info, err := os.Stat(layout.OperationLock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("operation lock mode = %04o, want 0600", info.Mode().Perm())
	}
	second, err := manager.BeginOperation(context.Background(), OperationSpec{Kind: OperationStart})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLockRejectsUnsafeFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, Layout)
		want  string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, layout Layout) {
				target := filepath.Join(filepath.Dir(layout.OperationLock), "target")
				writeTestFile(t, target, "", 0o600)
				if err := os.Symlink(target, layout.OperationLock); err != nil {
					t.Fatal(err)
				}
			},
			want: "open service operation lock",
		},
		{
			name: "directory",
			setup: func(t *testing.T, layout Layout) {
				if err := os.Mkdir(layout.OperationLock, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "open service operation lock",
		},
		{
			name: "mode",
			setup: func(t *testing.T, layout Layout) {
				writeTestFile(t, layout.OperationLock, "", 0o644)
			},
			want: "want 0600",
		},
		{
			name: "hard link",
			setup: func(t *testing.T, layout Layout) {
				writeTestFile(t, layout.OperationLock, "", 0o600)
				if err := os.Link(layout.OperationLock, layout.OperationLock+".link"); err != nil {
					t.Fatal(err)
				}
			},
			want: "link count",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := operationLockTestLayout(shortOperationLockRoot(t))
			test.setup(t, layout)
			_, err := operationLockTestManager(layout).BeginOperation(context.Background(), OperationSpec{Kind: OperationInstall})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BeginOperation() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateOperationLockFileRejectsUnexpectedOwner(t *testing.T) {
	layout := operationLockTestLayout(shortOperationLockRoot(t))
	writeTestFile(t, layout.OperationLock, "", 0o600)
	file, err := os.OpenFile(layout.OperationLock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
	if err := validateOperationLockFile(layout.OperationLock, file, os.Geteuid()+1); err == nil || !strings.Contains(err.Error(), "owned by UID") {
		t.Fatalf("validateOperationLockFile() error = %v, want owner rejection", err)
	}
}

func TestBeginOperationRejectsInvalidKindAndCanceledContext(t *testing.T) {
	manager := operationLockTestManager(operationLockTestLayout(shortOperationLockRoot(t)))
	if _, err := manager.BeginOperation(context.Background(), OperationSpec{Kind: "unknown"}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid kind error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.BeginOperation(ctx, OperationSpec{Kind: OperationStart}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
}

func shortOperationLockRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "tun-proxy-op-lock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func operationLockTestManager(layout Layout) *Manager {
	manager := NewManager(layout)
	manager.EffectiveUID = func() int { return 0 }
	manager.OwnerUID = os.Geteuid()
	return manager
}

func operationLockTestLayout(root string) Layout {
	return Layout{
		Label:         "com.example.tun-proxy",
		Binary:        filepath.Join(root, "bin", "tun-proxy"),
		Config:        filepath.Join(root, "etc", "config.yaml"),
		Plist:         filepath.Join(root, "LaunchDaemons", "com.example.tun-proxy.plist"),
		LogDirectory:  filepath.Join(root, "logs"),
		StandardOut:   filepath.Join(root, "logs", "stdout.log"),
		StandardErr:   filepath.Join(root, "logs", "stderr.log"),
		RuntimeDir:    filepath.Join(root, "run"),
		WorkerUser:    "_tun-proxy",
		WorkerGroup:   "_tun-proxy",
		WorkerDir:     filepath.Join(root, "run", "worker"),
		StatusSocket:  filepath.Join(root, "run", "worker", "status.sock"),
		DataDir:       filepath.Join(root, "lib"),
		State:         filepath.Join(root, "run", "state.json"),
		Lock:          filepath.Join(root, "run", "lock"),
		OperationLock: filepath.Join(root, "service-operation.lock"),
		FakeIPv4:      filepath.Join(root, "lib", "fake-ip.yaml"),
		FakeIPv6:      filepath.Join(root, "lib", "fake-ipv6.yaml"),
	}
}

func waitForOperationLockHelper(t *testing.T, ready string, stderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation-lock helper did not become ready: %s", stderr.String())
}
