//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/hailinpan/tun-proxy/internal/daemon"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
	"github.com/hailinpan/tun-proxy/internal/system"
	"golang.org/x/sys/unix"
)

func Cleanup(ctx context.Context, statePath, fallbackLockPath string) error {
	return CleanupWithStatusOwners(ctx, statePath, fallbackLockPath, uint32(os.Geteuid()))
}

// ClearManagedDNS resets network services that still point exclusively at the
// configured tun-proxy loopback resolver. It is a conservative recovery path
// for cases where no usable state record remains.
func ClearManagedDNS(ctx context.Context, replacement netip.Addr) ([]string, error) {
	if err := system.RequireRoot(); err != nil {
		return nil, err
	}
	return system.ClearDNSReplacement(ctx, system.NativeCommandRunner{}, replacement)
}

// CleanupWithStatusOwners performs crash recovery while restricting status
// socket removal to the explicitly trusted root/worker identities.
func CleanupWithStatusOwners(ctx context.Context, statePath, fallbackLockPath string, statusOwnerUIDs ...uint32) error {
	if err := system.RequireRoot(); err != nil {
		return err
	}
	state, err := system.ReadState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lockPath := state.LockFile
	if lockPath == "" {
		lockPath = fallbackLockPath
	}
	staleLock, lockErr := daemon.TryClaimStale(lockPath, state.PID)
	if errors.Is(lockErr, daemon.ErrLockHeld) {
		return fmt.Errorf("refuse cleanup: recorded instance lock %q is still held", lockPath)
	}
	if lockErr != nil && !errors.Is(lockErr, daemon.ErrLockNotFound) {
		return fmt.Errorf("inspect recorded instance lock: %w", lockErr)
	}
	if staleLock != nil {
		defer staleLock.Close() //nolint:errcheck // Best-effort cleanup.
	} else if err := unix.Kill(state.PID, 0); err == nil || errors.Is(err, unix.EPERM) {
		return fmt.Errorf("refuse cleanup: recorded process %d may still be running and its lock is missing", state.PID)
	} else if !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("check recorded process %d: %w", state.PID, err)
	}

	state.Phase = "cleaning"
	if err := system.WriteState(statePath, state); err != nil {
		return fmt.Errorf("mark cleanup in state file: %w", err)
	}
	if err := runtimestatus.RemoveStaleForOwners(state.StatusSocket, statusOwnerUIDs...); err != nil {
		return fmt.Errorf("status cleanup incomplete; state file retained: %w", err)
	}
	state.StatusSocket = ""
	if err := system.WriteState(statePath, state); err != nil {
		return fmt.Errorf("persist status cleanup progress: %w", err)
	}
	runner := system.NativeCommandRunner{}
	remainingDNS, dnsErr := system.RestoreDNS(ctx, runner, state.DNS)
	state.DNS = remainingDNS
	if writeErr := system.WriteState(statePath, state); writeErr != nil {
		return errors.Join(dnsErr, fmt.Errorf("persist DNS cleanup progress: %w", writeErr))
	}
	if dnsErr != nil {
		return fmt.Errorf("DNS cleanup incomplete; state file retained: %w", dnsErr)
	}

	if err := removeRecordedRoutes(ctx, runner, statePath, &state); err != nil {
		return fmt.Errorf("route cleanup incomplete; state file retained: %w", err)
	}
	if state.TUNName != "" {
		if _, err := net.InterfaceByName(state.TUNName); err == nil {
			return fmt.Errorf("recorded interface %q still exists; refusing to alter an interface without its owning file descriptor", state.TUNName)
		}
	}

	if staleLock != nil {
		err = staleLock.Remove()
	} else {
		err = daemon.RemoveStale(lockPath, state.PID)
	}
	if err != nil {
		return fmt.Errorf("lock cleanup incomplete; state file retained: %w", err)
	}
	if err := system.RemoveState(statePath); err != nil {
		return err
	}
	return nil
}
