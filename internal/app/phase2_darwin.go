//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/daemon"
	"github.com/hailinpan/tun-proxy/internal/system"
	internaltun "github.com/hailinpan/tun-proxy/internal/tun"
)

type lifecycleLock interface{ Close() error }
type lifecycleDevice interface {
	Name() string
	Close() error
}
type lifecyclePump interface {
	Run(context.Context) error
	Stats() internaltun.Stats
}

type phase2Dependencies struct {
	acquireLock func(string) (lifecycleLock, error)
	writeState  func(string, system.State) error
	removeState func(string) error
	createTUN   func(context.Context, config.TUN) (lifecycleDevice, error)
	addRoute    func(context.Context, system.RouteState) error
	verifyRoute func(context.Context, system.RouteState) error
	removeRoute func(context.Context, system.RouteState) error
	newPump     func(lifecycleDevice, int) (lifecyclePump, error)
}

// RunPhase2Probe creates a configured utun, installs only the Fake IP route,
// and counts packets until ctx is canceled. It intentionally does not alter
// DNS and does not claim to provide a usable proxy data plane.
func RunPhase2Probe(ctx context.Context, runtime *config.Config, configDigest string, onReady func(string)) (internaltun.Stats, error) {
	if err := system.RequireRoot(); err != nil {
		return internaltun.Stats{}, err
	}
	if _, err := os.Lstat(runtime.System.StateFile); err == nil {
		return internaltun.Stats{}, fmt.Errorf("recovery state %q already exists; run cleanup before starting", runtime.System.StateFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return internaltun.Stats{}, fmt.Errorf("inspect recovery state %q: %w", runtime.System.StateFile, err)
	}
	runner := system.NativeCommandRunner{}
	dependencies := phase2Dependencies{
		acquireLock: func(path string) (lifecycleLock, error) { return daemon.Acquire(path) },
		writeState:  system.WriteState,
		removeState: system.RemoveState,
		createTUN: func(ctx context.Context, settings config.TUN) (lifecycleDevice, error) {
			return internaltun.Create(ctx, settings, runner)
		},
		addRoute: func(ctx context.Context, route system.RouteState) error {
			return system.AddRoute(ctx, runner, route)
		},
		verifyRoute: func(ctx context.Context, route system.RouteState) error {
			return system.VerifyRoute(ctx, runner, route)
		},
		removeRoute: func(ctx context.Context, route system.RouteState) error {
			return system.RemoveRoute(ctx, runner, route)
		},
		newPump: func(device lifecycleDevice, mtu int) (lifecyclePump, error) {
			nativeDevice, ok := device.(*internaltun.Device)
			if !ok {
				return nil, errors.New("unexpected TUN device implementation")
			}
			pool, err := internaltun.NewBufferPool(internaltun.PacketOffset+mtu+256, 64)
			if err != nil {
				return nil, err
			}
			return internaltun.NewPump(nativeDevice, pool, func(context.Context, []byte) error { return nil })
		},
	}
	return runPhase2(ctx, runtime, configDigest, onReady, dependencies)
}

func runPhase2(ctx context.Context, runtime *config.Config, configDigest string, onReady func(string), dependencies phase2Dependencies) (stats internaltun.Stats, resultErr error) {
	if configDigest == "" {
		return stats, errors.New("config digest is required")
	}
	lock, err := dependencies.acquireLock(runtime.System.LockFile)
	if err != nil {
		return stats, err
	}

	state := system.NewState(configDigest)
	state.LockFile = runtime.System.LockFile
	stateExists := false
	var device lifecycleDevice
	cleanupNetwork := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var cleanupErrors []error
		if state.Route != nil {
			if err := dependencies.removeRoute(cleanupCtx, *state.Route); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("remove Fake IP route: %w", err))
			} else {
				state.Route = nil
				if stateExists {
					if err := dependencies.writeState(runtime.System.StateFile, state); err != nil {
						cleanupErrors = append(cleanupErrors, fmt.Errorf("persist route rollback: %w", err))
					}
				}
			}
		}
		if device != nil {
			if err := device.Close(); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("close %s: %w", device.Name(), err))
			}
		}
		return errors.Join(cleanupErrors...)
	}
	finalize := func(primary, cleanupErr error) error {
		if err := lock.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release process lock: %w", err))
		}
		if cleanupErr == nil && stateExists {
			if err := dependencies.removeState(runtime.System.StateFile); err != nil {
				cleanupErr = err
			}
		}
		return errors.Join(primary, cleanupErr)
	}

	if err := dependencies.writeState(runtime.System.StateFile, state); err != nil {
		return stats, errors.Join(err, lock.Close())
	}
	stateExists = true
	device, err = dependencies.createTUN(ctx, runtime.TUN)
	if err != nil {
		return stats, finalize(err, cleanupNetwork())
	}
	state.TUNName = device.Name()
	if err := dependencies.writeState(runtime.System.StateFile, state); err != nil {
		return stats, finalize(err, cleanupNetwork())
	}

	route := system.RouteState{Prefix: runtime.FakeIP.Prefix.String(), Interface: device.Name()}
	state.Route = &route
	if err := dependencies.writeState(runtime.System.StateFile, state); err != nil {
		return stats, finalize(err, cleanupNetwork())
	}
	if err := dependencies.addRoute(ctx, route); err != nil {
		return stats, finalize(err, cleanupNetwork())
	}
	if err := dependencies.verifyRoute(ctx, route); err != nil {
		return stats, finalize(err, cleanupNetwork())
	}
	pump, err := dependencies.newPump(device, runtime.TUN.MTU)
	if err != nil {
		return stats, finalize(err, cleanupNetwork())
	}
	state.Phase = "phase2-running"
	if err := dependencies.writeState(runtime.System.StateFile, state); err != nil {
		return stats, finalize(err, cleanupNetwork())
	}
	pumpCtx, cancelPump := context.WithCancel(context.Background())
	pumpDone := make(chan error, 1)
	go func() { pumpDone <- pump.Run(pumpCtx) }()
	if onReady != nil {
		onReady(device.Name())
	}
	var primary error
	pumpFinished := false
	select {
	case <-ctx.Done():
	case err := <-pumpDone:
		pumpFinished = true
		if err != nil {
			primary = err
		} else {
			primary = errors.New("TUN packet pump stopped unexpectedly")
		}
	}
	state.Phase = "stopping"
	if err := dependencies.writeState(runtime.System.StateFile, state); err != nil {
		primary = errors.Join(primary, err)
	}

	// Network cleanup removes the route before closing the device.
	cleanupErr := cleanupNetwork()
	cancelPump()
	if !pumpFinished {
		select {
		case err := <-pumpDone:
			if err != nil {
				primary = errors.Join(primary, err)
			}
		case <-time.After(2 * time.Second):
			cleanupErr = errors.Join(cleanupErr, errors.New("TUN packet pump did not stop within 2s"))
		}
	}
	stats = pump.Stats()
	return stats, finalize(primary, cleanupErr)
}
