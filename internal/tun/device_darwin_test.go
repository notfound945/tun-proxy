//go:build darwin

package tun

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/system"
	wgtun "golang.zx2c4.com/wireguard/tun"
)

type failingRunner struct{ err error }

func (runner failingRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, runner.err
}

func TestCreateClosesDeviceWhenConfigurationFails(t *testing.T) {
	native := newFakeNativeDevice("utun9")
	settings := config.TUN{Address: netip.MustParseAddr("10.255.0.2"), Peer: netip.MustParseAddr("10.255.0.1"), MTU: 1400}
	_, err := createWithFactory(t.Context(), settings, failingRunner{err: errors.New("ifconfig failed")}, func(string, int) (wgtun.Device, error) {
		return native, nil
	})
	if err == nil || !strings.Contains(err.Error(), "configure utun9") {
		t.Fatalf("createWithFactory() = %v", err)
	}
	select {
	case <-native.closed:
	default:
		t.Fatal("native device was not closed after configuration failure")
	}
}

var _ system.CommandRunner = failingRunner{}

type sequenceRunner struct {
	calls int
	fail  int
}

func (runner *sequenceRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	runner.calls++
	if runner.calls == runner.fail {
		return nil, errors.New("ifconfig failed")
	}
	return nil, nil
}

func TestCreateConfiguresIPv6AndClosesOnSecondFailure(t *testing.T) {
	native := newFakeNativeDevice("utun9")
	settings := config.TUN{
		Address: netip.MustParseAddr("10.255.0.2"), Peer: netip.MustParseAddr("10.255.0.1"),
		IPv6Address: netip.MustParseAddr("fd00:ffff::2"), IPv6Peer: netip.MustParseAddr("fd00:ffff::1"), MTU: 1400,
	}
	runner := &sequenceRunner{fail: 2}
	_, err := createWithFactory(t.Context(), settings, runner, func(string, int) (wgtun.Device, error) { return native, nil })
	if err == nil || !strings.Contains(err.Error(), "configure IPv6") || runner.calls != 2 {
		t.Fatalf("createWithFactory() = %v, calls = %d", err, runner.calls)
	}
	select {
	case <-native.closed:
	default:
		t.Fatal("native device was not closed after IPv6 configuration failure")
	}
}

func TestOpenFileWrapsDeviceWithoutConfiguration(t *testing.T) {
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeFile.Close()
	native := newFakeNativeDevice("utun12")
	device, err := openFileWithFactory(readFile, func(file *os.File, mtu int) (wgtun.Device, error) {
		if file != readFile {
			t.Fatalf("factory file = %p, want %p", file, readFile)
		}
		if mtu != 0 {
			t.Fatalf("factory MTU = %d, want 0", mtu)
		}
		return native, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if device.Name() != "utun12" || device.Native() != native {
		t.Fatalf("opened device = name %q native %T", device.Name(), device.Native())
	}
	if err := device.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenFileClosesDescriptorWhenFactoryFails(t *testing.T) {
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeFile.Close()
	_, err = openFileWithFactory(readFile, func(*os.File, int) (wgtun.Device, error) {
		return nil, errors.New("route socket denied")
	})
	if err == nil || !strings.Contains(err.Error(), "route socket denied") {
		t.Fatalf("openFileWithFactory() = %v", err)
	}
	if _, err := readFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("inherited descriptor remains open: %v", err)
	}
}

func TestOpenFileClosesNativeWhenNameFails(t *testing.T) {
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeFile.Close()
	native := newFakeNativeDevice("")
	device, err := openFileWithFactory(readFile, func(*os.File, int) (wgtun.Device, error) {
		return &nameFailingDevice{fakeNativeDevice: native}, nil
	})
	if err == nil || device != nil || !strings.Contains(err.Error(), "read inherited utun name") {
		t.Fatalf("openFileWithFactory() = (%v, %v)", device, err)
	}
	select {
	case <-native.closed:
	default:
		t.Fatal("native device was not closed after name failure")
	}
}

type nameFailingDevice struct{ *fakeNativeDevice }

func (*nameFailingDevice) Name() (string, error) { return "", errors.New("name unavailable") }
