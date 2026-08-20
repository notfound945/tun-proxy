package launchservice

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maxInstalledFileSize = 512 << 20

func ensureDirectory(path string, mode os.FileMode, expectedUID int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("directory must be a clean absolute path: %q", path)
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("create directory %q: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("service directory %q is not a real directory", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != expectedUID {
		return fmt.Errorf("service directory %q is not owned by UID %d", path, expectedUID)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod directory %q: %w", path, err)
	}
	return nil
}

func inspectSource(path string, maxSize int64) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("source must be a clean absolute path: %q", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect source %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("source %q is not a regular file", path)
	}
	if info.Size() > maxSize {
		return nil, fmt.Errorf("source %q exceeds %d bytes", path, maxSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open source %q: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect opened source %q: %w", path, err)
	}
	if !os.SameFile(info, opened) {
		file.Close()
		return nil, fmt.Errorf("source %q changed while opening", path)
	}
	return file, nil
}

func stageCopy(source, target string, mode os.FileMode, maxSize int64) (string, error) {
	sourceFile, err := inspectSource(source, maxSize)
	if err != nil {
		return "", err
	}
	defer sourceFile.Close()
	temporary, err := os.CreateTemp(filepath.Dir(target), ".tun-proxy-stage-*")
	if err != nil {
		return "", fmt.Errorf("create staged file for %q: %w", target, err)
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	written, err := io.Copy(temporary, io.LimitReader(sourceFile, maxSize+1))
	if err != nil {
		return "", fmt.Errorf("stage %q: %w", target, err)
	}
	if written > maxSize {
		return "", fmt.Errorf("source %q exceeds %d bytes", source, maxSize)
	}
	if err := temporary.Chmod(mode); err != nil {
		return "", fmt.Errorf("chmod staged %q: %w", target, err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync staged %q: %w", target, err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close staged %q: %w", target, err)
	}
	cleanup = false
	return temporaryPath, nil
}

func stageContents(contents []byte, target string, mode os.FileMode) (string, error) {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".tun-proxy-stage-*")
	if err != nil {
		return "", fmt.Errorf("create staged file for %q: %w", target, err)
	}
	path := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if _, err := temporary.Write(contents); err != nil {
		return "", fmt.Errorf("write staged %q: %w", target, err)
	}
	if err := temporary.Chmod(mode); err != nil {
		return "", fmt.Errorf("chmod staged %q: %w", target, err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync staged %q: %w", target, err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close staged %q: %w", target, err)
	}
	cleanup = false
	return path, nil
}

type replacement struct {
	target string
	backup string
	active bool
}

func activateStage(stage, target string, refuseExisting bool) (replacement, error) {
	result := replacement{target: target}
	if info, err := os.Lstat(target); err == nil {
		if refuseExisting {
			return result, fmt.Errorf("service artifact %q already exists; run %q", target, UpgradeCommand)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return result, fmt.Errorf("service artifact %q is not a regular file", target)
		}
		backup, err := os.CreateTemp(filepath.Dir(target), ".tun-proxy-backup-*")
		if err != nil {
			return result, fmt.Errorf("reserve backup for %q: %w", target, err)
		}
		result.backup = backup.Name()
		if err := backup.Close(); err != nil {
			return result, err
		}
		if err := os.Remove(result.backup); err != nil {
			return result, err
		}
		if err := os.Rename(target, result.backup); err != nil {
			return result, fmt.Errorf("backup %q: %w", target, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("inspect service artifact %q: %w", target, err)
	}
	if err := os.Rename(stage, target); err != nil {
		if result.backup != "" {
			_ = os.Rename(result.backup, target)
		}
		return replacement{}, fmt.Errorf("activate %q: %w", target, err)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		_ = os.Remove(target)
		if result.backup != "" {
			_ = os.Rename(result.backup, target)
		}
		return replacement{}, fmt.Errorf("sync activated %q: %w", target, err)
	}
	result.active = true
	return result, nil
}

func (item *replacement) rollback() error {
	if !item.active {
		return nil
	}
	var failures []error
	if err := os.Remove(item.target); err != nil && !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, err)
	}
	if item.backup != "" {
		if err := os.Rename(item.backup, item.target); err != nil {
			failures = append(failures, err)
		}
	}
	if err := syncDirectory(filepath.Dir(item.target)); err != nil {
		failures = append(failures, err)
	}
	item.active = false
	return errors.Join(failures...)
}

func (item *replacement) commit() error {
	if item.backup == "" {
		return nil
	}
	err := os.Remove(item.backup)
	if err == nil {
		err = syncDirectory(filepath.Dir(item.target))
	}
	item.backup = ""
	return err
}

type removal struct {
	target    string
	tombstone string
	active    bool
}

func stageRemoval(target string) (removal, error) {
	result := removal{target: target}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("inspect service artifact %q: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return result, fmt.Errorf("refuse to remove non-regular service artifact %q", target)
	}
	tombstone, err := os.CreateTemp(filepath.Dir(target), ".tun-proxy-remove-*")
	if err != nil {
		return result, fmt.Errorf("reserve removal tombstone for %q: %w", target, err)
	}
	result.tombstone = tombstone.Name()
	if err := tombstone.Close(); err != nil {
		return result, fmt.Errorf("close removal tombstone for %q: %w", target, err)
	}
	if err := os.Remove(result.tombstone); err != nil {
		return result, fmt.Errorf("prepare removal tombstone for %q: %w", target, err)
	}
	if err := os.Rename(target, result.tombstone); err != nil {
		return result, fmt.Errorf("stage removal of %q: %w", target, err)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		_ = os.Rename(result.tombstone, target)
		return removal{}, fmt.Errorf("sync staged removal of %q: %w", target, err)
	}
	result.active = true
	return result, nil
}

func (item *removal) rollback() error {
	if !item.active {
		return nil
	}
	if err := os.Rename(item.tombstone, item.target); err != nil {
		return fmt.Errorf("restore removed service artifact %q: %w", item.target, err)
	}
	item.active = false
	return syncDirectory(filepath.Dir(item.target))
}

func (item *removal) commit() error {
	if !item.active {
		return nil
	}
	if err := os.Remove(item.tombstone); err != nil {
		return fmt.Errorf("delete removed service artifact %q: %w", item.target, err)
	}
	item.active = false
	return syncDirectory(filepath.Dir(item.target))
}

// commitRemovals crosses the irreversible uninstall boundary. Every target has
// already been renamed away before this function is called, so cleanup failures
// leave the service uninstalled and are aggregated instead of triggering a
// rollback that can no longer restore earlier tombstones.
func commitRemovals(removals []removal) error {
	var failures []error
	for index := range removals {
		failures = append(failures, removals[index].commit())
	}
	return errors.Join(failures...)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
