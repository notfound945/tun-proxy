//go:build darwin

package system

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
)

// ConfigureTUN assigns the point-to-point IPv4 addresses and brings the utun
// interface up. Interface creation and destruction remain owned by the tun
// package's file descriptor.
func ConfigureTUN(ctx context.Context, runner CommandRunner, name string, address, peer netip.Addr, mtu int) error {
	if !validSystemInterface(name) || len(name) < 5 || name[:4] != "utun" {
		return fmt.Errorf("invalid utun interface %q", name)
	}
	if !address.Is4() || !peer.Is4() || address == peer {
		return fmt.Errorf("invalid point-to-point addresses %s and %s", address, peer)
	}
	if mtu < 576 || mtu > 9000 {
		return fmt.Errorf("invalid TUN MTU %d", mtu)
	}
	_, err := runner.Run(ctx, "/sbin/ifconfig",
		name,
		"inet", address.String(), peer.String(),
		"netmask", "255.255.255.255",
		"mtu", strconv.Itoa(mtu),
		"up",
	)
	if err != nil {
		return fmt.Errorf("configure %s: %w", name, err)
	}
	return nil
}

// ConfigureTUNIPv6 adds the point-to-point IPv6 pair after the base IPv4
// configuration has brought the utun interface up.
func ConfigureTUNIPv6(ctx context.Context, runner CommandRunner, name string, address, peer netip.Addr) error {
	if !validSystemInterface(name) || len(name) < 5 || name[:4] != "utun" {
		return fmt.Errorf("invalid utun interface %q", name)
	}
	if !address.Is6() || address.Is4In6() || !peer.Is6() || peer.Is4In6() || address == peer {
		return fmt.Errorf("invalid IPv6 point-to-point addresses %s and %s", address, peer)
	}
	_, err := runner.Run(ctx, "/sbin/ifconfig",
		name, "inet6", address.String(), peer.String(), "prefixlen", "128",
	)
	if err != nil {
		return fmt.Errorf("configure IPv6 on %s: %w", name, err)
	}
	return nil
}
