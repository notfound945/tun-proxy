//go:build darwin

package tun

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/system"
	wgtun "golang.zx2c4.com/wireguard/tun"
)

type Device struct {
	native    wgtun.Device
	name      string
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

type DeviceFactory func(name string, mtu int) (wgtun.Device, error)
type FileDeviceFactory func(file *os.File, mtu int) (wgtun.Device, error)

func Create(ctx context.Context, settings config.TUN, runner system.CommandRunner) (*Device, error) {
	return createWithFactory(ctx, settings, runner, wgtun.CreateTUN)
}

// OpenFile reconstructs a utun device from an inherited descriptor without
// changing interface configuration. It always takes ownership of file,
// including when construction fails.
func OpenFile(file *os.File) (*Device, error) {
	return openFileWithFactory(file, wgtun.CreateTUNFromFile)
}

func openFileWithFactory(file *os.File, factory FileDeviceFactory) (*Device, error) {
	if file == nil {
		return nil, fmt.Errorf("open inherited utun: nil file")
	}
	native, err := factory(file, 0)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("open inherited utun: %w", err)
	}
	device := &Device{native: native}
	name, err := native.Name()
	if err != nil {
		_ = device.Close()
		return nil, fmt.Errorf("read inherited utun name: %w", err)
	}
	device.name = name
	return device, nil
}

func createWithFactory(ctx context.Context, settings config.TUN, runner system.CommandRunner, factory DeviceFactory) (*Device, error) {
	native, err := factory("utun", settings.MTU)
	if err != nil {
		return nil, fmt.Errorf("create utun: %w", err)
	}
	device := &Device{native: native}
	name, err := native.Name()
	if err != nil {
		_ = device.Close()
		return nil, fmt.Errorf("read utun name: %w", err)
	}
	device.name = name
	if err := system.ConfigureTUN(ctx, runner, name, settings.Address, settings.Peer, settings.MTU); err != nil {
		closeErr := device.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("%w; additionally close %s: %v", err, name, closeErr)
		}
		return nil, err
	}
	if settings.IPv6Address.IsValid() {
		if err := system.ConfigureTUNIPv6(ctx, runner, name, settings.IPv6Address, settings.IPv6Peer); err != nil {
			closeErr := device.Close()
			if closeErr != nil {
				return nil, fmt.Errorf("%w; additionally close %s: %v", err, name, closeErr)
			}
			return nil, err
		}
	}
	return device, nil
}

func (device *Device) Name() string         { return device.name }
func (device *Device) Native() wgtun.Device { return device.native }
func (device *Device) Closed() bool         { return device.closed.Load() }

func (device *Device) Close() error {
	device.closeOnce.Do(func() {
		device.closed.Store(true)
		device.closeErr = device.native.Close()
	})
	return device.closeErr
}
