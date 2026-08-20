//go:build darwin

// Package daemon contains process lifecycle primitives.
package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	ErrLockNotFound = errors.New("daemon lock file is missing")
	ErrLockHeld     = errors.New("daemon lock is held")
)

type LockState uint8

const (
	LockMissing LockState = iota
	LockHeld
	LockStale
)

type Lock struct {
	mutex sync.Mutex
	path  string
	file  *os.File
}

// StaleLock is an unlocked, PID-validated lock inode claimed exclusively for
// the duration of crash recovery.
type StaleLock struct {
	mutex sync.Mutex
	path  string
	file  *os.File
}

// ProbeLock distinguishes a live lock holder from an unlocked stale inode.
// The PID stored in an unlocked inode must match the recovery state.
func ProbeLock(path string, expectedPID int) (LockState, error) {
	claim, err := TryClaimStale(path, expectedPID)
	if errors.Is(err, ErrLockNotFound) {
		return LockMissing, nil
	}
	if errors.Is(err, ErrLockHeld) {
		return LockHeld, nil
	}
	if err != nil {
		return LockMissing, err
	}
	if err := claim.Close(); err != nil {
		return LockMissing, err
	}
	return LockStale, nil
}

// TryClaimStale opens and exclusively locks an existing stale lock file. The
// caller must Close or Remove the returned claim.
func TryClaimStale(path string, expectedPID int) (*StaleLock, error) {
	if path == "" {
		return nil, ErrLockNotFound
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("lock path must be a clean absolute path: %q", path)
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, ErrLockNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open stale lock %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect stale lock %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("refuse insecure stale lock %q", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("refuse stale lock %q owned by UID %d", path, stat.Uid)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLockHeld, path)
		}
		return nil, fmt.Errorf("claim stale lock %q: %w", path, err)
	}
	contents, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("read stale lock %q: %w", path, err)
	}
	if len(contents) > 32 {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("refuse oversized lock file %q", path)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid != expectedPID {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("refuse to claim lock %q: recorded PID does not match state PID %d", path, expectedPID)
	}
	closeFile = false
	return &StaleLock{path: path, file: file}, nil
}

// RemoveStale removes an unlocked lock file only when its recorded PID matches
// the recovery state. It never removes a lock held by a live process.
func RemoveStale(path string, expectedPID int) error {
	claim, err := TryClaimStale(path, expectedPID)
	if errors.Is(err, ErrLockNotFound) {
		return nil
	}
	if errors.Is(err, ErrLockHeld) {
		return fmt.Errorf("refuse to remove held lock %q: %w", path, err)
	}
	if err != nil {
		return err
	}
	return claim.Remove()
}

// Remove unlinks the claimed inode and releases the exclusive recovery lock.
func (lock *StaleLock) Remove() error {
	lock.mutex.Lock()
	defer lock.mutex.Unlock()
	if lock.file == nil {
		return nil
	}
	var failures []error
	pathInfo, pathErr := os.Lstat(lock.path)
	fileInfo, fileErr := lock.file.Stat()
	if pathErr != nil {
		failures = append(failures, fmt.Errorf("inspect stale lock path: %w", pathErr))
	} else if fileErr != nil {
		failures = append(failures, fmt.Errorf("inspect stale lock file: %w", fileErr))
	} else if !os.SameFile(pathInfo, fileInfo) {
		failures = append(failures, fmt.Errorf("refuse to remove replaced lock %q", lock.path))
	} else if err := os.Remove(lock.path); err != nil {
		failures = append(failures, fmt.Errorf("remove stale lock %q: %w", lock.path, err))
	}
	failures = append(failures, lock.closeLocked())
	return errors.Join(failures...)
}

// Close releases a stale claim without removing its inode.
func (lock *StaleLock) Close() error {
	lock.mutex.Lock()
	defer lock.mutex.Unlock()
	return lock.closeLocked()
}

func (lock *StaleLock) closeLocked() error {
	if lock.file == nil {
		return nil
	}
	var failures []error
	if err := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN); err != nil {
		failures = append(failures, fmt.Errorf("unlock stale lock: %w", err))
	}
	if err := lock.file.Close(); err != nil {
		failures = append(failures, fmt.Errorf("close stale lock: %w", err))
	}
	lock.file = nil
	return errors.Join(failures...)
}

func Acquire(path string) (*Lock, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("lock path must be a clean absolute path: %q", path)
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect lock file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("lock path %q is not a regular file", path)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure lock file permissions: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another tun-proxy instance holds %q", path)
		}
		return nil, fmt.Errorf("lock %q: %w", path, err)
	}

	if err := file.Truncate(0); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("seek lock file: %w", err)
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("write lock owner: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("sync lock file: %w", err)
	}

	closeOnError = false
	return &Lock{path: path, file: file}, nil
}

// Close unlinks this lock inode before releasing it. A new process may then
// create the next lock file, while no process can accidentally acquire this
// retiring inode between unlock and removal.
func (lock *Lock) Close() error {
	lock.mutex.Lock()
	defer lock.mutex.Unlock()
	if lock.file == nil {
		return nil
	}

	var failures []error
	pathInfo, pathErr := os.Lstat(lock.path)
	fileInfo, fileErr := lock.file.Stat()
	if pathErr == nil && fileErr == nil && os.SameFile(pathInfo, fileInfo) {
		if err := os.Remove(lock.path); err != nil {
			failures = append(failures, fmt.Errorf("remove lock file: %w", err))
		}
	} else if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
		failures = append(failures, fmt.Errorf("inspect lock path: %w", pathErr))
	} else if fileErr != nil {
		failures = append(failures, fmt.Errorf("inspect held lock: %w", fileErr))
	}

	if err := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN); err != nil {
		failures = append(failures, fmt.Errorf("unlock: %w", err))
	}
	if err := lock.file.Close(); err != nil {
		failures = append(failures, fmt.Errorf("close lock: %w", err))
	}
	lock.file = nil
	return errors.Join(failures...)
}
