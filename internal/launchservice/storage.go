package launchservice

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hailinpan/tun-proxy/internal/privsep"
)

type metadataSnapshot struct {
	path    string
	uid     int
	gid     int
	mode    os.FileMode
	created bool
	dir     bool
}

type storageTransaction struct {
	changes     []metadataSnapshot
	absentFiles []string
	workerUID   int
	workerGID   int
	active      bool
}

func prepareWorkerStorage(layout Layout, rootUID int, identity privsep.Identity) (*storageTransaction, error) {
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	if identity.User != layout.WorkerUser || identity.Group != layout.WorkerGroup || identity.UID == 0 || identity.GID == 0 {
		return nil, fmt.Errorf("invalid managed worker identity %+v", identity)
	}
	transaction := &storageTransaction{workerUID: int(identity.UID), workerGID: int(identity.GID), active: true}
	fail := func(err error) (*storageTransaction, error) {
		return nil, errors.Join(err, transaction.rollback())
	}
	for _, path := range []string{layout.WorkerDir, layout.DataDir} {
		if err := transaction.prepareDirectory(path, 0o700, rootUID); err != nil {
			return fail(err)
		}
	}
	for _, path := range []string{layout.FakeIPv4, layout.FakeIPv6} {
		if err := transaction.prepareOptionalFile(path, 0o600, rootUID); err != nil {
			return fail(err)
		}
	}
	return transaction, nil
}

func validateWorkerStorage(layout Layout, identity privsep.Identity) error {
	if err := layout.Validate(); err != nil {
		return err
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
		dir  bool
	}{
		{layout.WorkerDir, 0o700, true}, {layout.DataDir, 0o700, true},
		{layout.FakeIPv4, 0o600, false}, {layout.FakeIPv6, 0o600, false},
	} {
		info, err := os.Lstat(item.path)
		if errors.Is(err, os.ErrNotExist) && !item.dir {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect worker storage %q: %w", item.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || item.dir != info.IsDir() || (!item.dir && !info.Mode().IsRegular()) {
			return fmt.Errorf("worker storage %q has unsafe file type %v", item.path, info.Mode())
		}
		uid, gid, err := fileOwner(info)
		if err != nil {
			return fmt.Errorf("inspect worker storage owner %q: %w", item.path, err)
		}
		if uid != int(identity.UID) || gid != int(identity.GID) || info.Mode().Perm() != item.mode {
			return fmt.Errorf("worker storage %q has uid=%d gid=%d mode=%04o, want uid=%d gid=%d mode=%04o",
				item.path, uid, gid, info.Mode().Perm(), identity.UID, identity.GID, item.mode)
		}
	}
	return nil
}

// ValidateWorkerStorage verifies the installed worker-owned runtime and
// persistence locations without changing their metadata.
func ValidateWorkerStorage(layout Layout, identity privsep.Identity) error {
	return validateWorkerStorage(layout, identity)
}

func (transaction *storageTransaction) prepareDirectory(path string, mode os.FileMode, rootUID int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("worker directory must be a clean absolute path: %q", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil {
			return fmt.Errorf("create worker directory %q: %w", path, err)
		}
		transaction.changes = append(transaction.changes, metadataSnapshot{path: path, created: true, dir: true})
		if err := os.Chown(path, transaction.workerUID, transaction.workerGID); err != nil {
			return fmt.Errorf("chown worker directory %q: %w", path, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("chmod worker directory %q: %w", path, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect worker directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("worker directory %q is not a real directory", path)
	}
	uid, gid, err := fileOwner(info)
	if err != nil {
		return err
	}
	if uid != rootUID && uid != transaction.workerUID {
		return fmt.Errorf("refuse to migrate worker directory %q owned by UID %d", path, uid)
	}
	transaction.changes = append(transaction.changes, metadataSnapshot{path: path, uid: uid, gid: gid, mode: info.Mode().Perm(), dir: true})
	if err := os.Chown(path, transaction.workerUID, transaction.workerGID); err != nil {
		return fmt.Errorf("chown worker directory %q: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod worker directory %q: %w", path, err)
	}
	return nil
}

func (transaction *storageTransaction) prepareOptionalFile(path string, mode os.FileMode, rootUID int) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		transaction.absentFiles = append(transaction.absentFiles, path)
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect worker persistence %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("worker persistence %q is not a regular file", path)
	}
	uid, gid, err := fileOwner(info)
	if err != nil {
		return err
	}
	if uid != rootUID && uid != transaction.workerUID {
		return fmt.Errorf("refuse to migrate worker persistence %q owned by UID %d", path, uid)
	}
	transaction.changes = append(transaction.changes, metadataSnapshot{path: path, uid: uid, gid: gid, mode: info.Mode().Perm()})
	if err := os.Chown(path, transaction.workerUID, transaction.workerGID); err != nil {
		return fmt.Errorf("chown worker persistence %q: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod worker persistence %q: %w", path, err)
	}
	return nil
}

func (transaction *storageTransaction) commit() { transaction.active = false }

func (transaction *storageTransaction) rollback() error {
	if transaction == nil || !transaction.active {
		return nil
	}
	transaction.active = false
	var failures []error
	for _, path := range transaction.absentFiles {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect new worker persistence %q during rollback: %w", path, err))
			continue
		}
		uid, gid, ownerErr := fileOwner(info)
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || ownerErr != nil || uid != transaction.workerUID || gid != transaction.workerGID {
			failures = append(failures, fmt.Errorf("refuse to remove unrecognized worker persistence %q during rollback", path))
			continue
		}
		if err := os.Remove(path); err != nil {
			failures = append(failures, fmt.Errorf("remove new worker persistence %q during rollback: %w", path, err))
		}
	}
	for index := len(transaction.changes) - 1; index >= 0; index-- {
		change := transaction.changes[index]
		if change.created {
			if err := os.Remove(change.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("remove created worker directory %q during rollback: %w", change.path, err))
			}
			continue
		}
		if err := os.Chown(change.path, change.uid, change.gid); err != nil {
			failures = append(failures, fmt.Errorf("restore owner of %q: %w", change.path, err))
			continue
		}
		if err := os.Chmod(change.path, change.mode); err != nil {
			failures = append(failures, fmt.Errorf("restore mode of %q: %w", change.path, err))
		}
	}
	return errors.Join(failures...)
}

func fileOwner(info os.FileInfo) (int, int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("file metadata does not expose POSIX ownership")
	}
	return int(stat.Uid), int(stat.Gid), nil
}
