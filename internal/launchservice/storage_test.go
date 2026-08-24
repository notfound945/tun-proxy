package launchservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/privsep"
)

func TestPrepareWorkerStorageMigratesAndRollsBackMetadata(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(layout.RuntimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(layout.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(layout.DataDir, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, layout.FakeIPv4, "map", 0o644)
	if err := os.Chown(layout.FakeIPv4, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	identity := currentTestIdentity(layout)
	transaction, err := prepareWorkerStorage(layout, os.Geteuid(), identity)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectoryMode(t, layout.WorkerDir, 0o700)
	assertDirectoryMode(t, layout.DataDir, 0o700)
	assertFileMode(t, layout.FakeIPv4, 0o600)
	if err := validateWorkerStorage(layout, identity); err != nil {
		t.Fatal(err)
	}
	if err := transaction.rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(layout.WorkerDir); !os.IsNotExist(err) {
		t.Fatalf("created worker directory remains: %v", err)
	}
	assertDirectoryMode(t, layout.DataDir, 0o755)
	assertFileMode(t, layout.FakeIPv4, 0o644)
}

func TestWorkerStorageRollbackRemovesNewPersistence(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(layout.RuntimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	identity := currentTestIdentity(layout)
	transaction, err := prepareWorkerStorage(layout, os.Geteuid(), identity)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, layout.FakeIPv4, "new map", 0o600)
	if err := transaction.rollback(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{layout.FakeIPv4, layout.WorkerDir, layout.DataDir} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("rollback residue %q: %v", path, err)
		}
	}
}

func TestPrepareWorkerStorageRejectsSymlinkPersistence(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(layout.RuntimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(layout.FakeIPv4), "operator-file")
	writeTestFile(t, target, "keep", 0o600)
	if err := os.Symlink(target, layout.FakeIPv4); err != nil {
		t.Fatal(err)
	}
	_, err := prepareWorkerStorage(layout, os.Geteuid(), currentTestIdentity(layout))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("prepareWorkerStorage() error = %v", err)
	}
	if got := readTestFile(t, target); got != "keep" {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func TestPrepareEphemeralWorkerRuntimeRecreatesMissingDirectory(t *testing.T) {
	layout := testLayout(t)
	identity := currentTestIdentity(layout)
	if err := PrepareEphemeralWorkerRuntime(layout, os.Geteuid(), identity); err != nil {
		t.Fatal(err)
	}
	assertDirectoryMode(t, layout.RuntimeDir, 0o755)
	assertDirectoryMode(t, layout.WorkerDir, 0o700)
	info, err := os.Lstat(layout.WorkerDir)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, err := fileOwner(info)
	if err != nil {
		t.Fatal(err)
	}
	if uid != int(identity.UID) || gid != int(identity.GID) {
		t.Fatalf("worker runtime owner = %d:%d, want %d:%d", uid, gid, identity.UID, identity.GID)
	}
	if _, err := os.Lstat(layout.DataDir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral runtime preparation changed persistent data directory: %v", err)
	}
}

func TestPrepareEphemeralWorkerRuntimeAcceptsValidExistingDirectory(t *testing.T) {
	layout := testLayout(t)
	identity := currentTestIdentity(layout)
	if err := PrepareEphemeralWorkerRuntime(layout, os.Geteuid(), identity); err != nil {
		t.Fatal(err)
	}
	if err := PrepareEphemeralWorkerRuntime(layout, os.Geteuid(), identity); err != nil {
		t.Fatalf("second preparation failed: %v", err)
	}
}

func TestPrepareEphemeralWorkerRuntimeRejectsUnsafeExistingDirectory(t *testing.T) {
	t.Run("mode", func(t *testing.T) {
		layout := testLayout(t)
		identity := currentTestIdentity(layout)
		if err := PrepareEphemeralWorkerRuntime(layout, os.Geteuid(), identity); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(layout.WorkerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		err := PrepareEphemeralWorkerRuntime(layout, os.Geteuid(), identity)
		if err == nil || !strings.Contains(err.Error(), "mode=0755") {
			t.Fatalf("PrepareEphemeralWorkerRuntime() error = %v", err)
		}
		assertDirectoryMode(t, layout.WorkerDir, 0o755)
	})

	t.Run("symlink", func(t *testing.T) {
		layout := testLayout(t)
		if err := os.MkdirAll(layout.RuntimeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(layout.RuntimeDir, "operator-directory")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, layout.WorkerDir); err != nil {
			t.Fatal(err)
		}
		err := PrepareEphemeralWorkerRuntime(layout, os.Geteuid(), currentTestIdentity(layout))
		if err == nil || !strings.Contains(err.Error(), "unsafe file type") {
			t.Fatalf("PrepareEphemeralWorkerRuntime() error = %v", err)
		}
		if info, statErr := os.Lstat(target); statErr != nil || !info.IsDir() {
			t.Fatalf("symlink target changed: info=%v err=%v", info, statErr)
		}
	})
}

func currentTestIdentity(layout Layout) privsep.Identity {
	return privsep.Identity{
		User: layout.WorkerUser, Group: layout.WorkerGroup,
		UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()), Home: privsep.ProductionHome,
	}
}
