//go:build darwin

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockEnforcesSingleInstanceAndCanBeReacquired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tun-proxy.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		t.Fatal("lock file does not contain a PID")
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("second Acquire succeeded while lock was held")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("lock path still exists after Close: %v", err)
	}

	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveStaleValidatesPID(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tun-proxy.lock")
	if err := os.WriteFile(path, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStale(path, 54321); err == nil {
		t.Fatal("RemoveStale removed a lock with a mismatched PID")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mismatched lock was removed: %v", err)
	}
	if err := RemoveStale(path, 12345); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("matching stale lock still exists: %v", err)
	}
}

func TestRemoveStaleRefusesHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tun-proxy.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // Best-effort cleanup.
	if err := RemoveStale(path, os.Getpid()); err == nil {
		t.Fatal("RemoveStale removed a held lock")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(contents)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("held lock contents changed: %q", contents)
	}
}

func TestProbeLockTreatsUnlockedLivePIDAsStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tun-proxy.lock")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := ProbeLock(path, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if state != LockStale {
		t.Fatalf("ProbeLock state = %v, want stale", state)
	}
	claim, err := TryClaimStale(path, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close() //nolint:errcheck // Best-effort cleanup.
	if _, err := TryClaimStale(path, os.Getpid()); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second stale claim error = %v, want held", err)
	}
}

func TestLockRefusesSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(link); err == nil {
		t.Fatal("Acquire accepted a symlink")
	}
}
