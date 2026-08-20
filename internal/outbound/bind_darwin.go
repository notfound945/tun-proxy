//go:build darwin

// Package outbound contains socket creation primitives that force traffic onto
// an explicitly selected physical interface.
package outbound

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// InterfaceControl returns a net.Dialer control hook that applies
// IP_BOUND_IF before connect(2). The interface is resolved for every new
// socket so removal or renumbering is reported instead of silently falling
// back to the default route.
func InterfaceControl(interfaceName string) func(string, string, syscall.RawConn) error {
	return func(network, _ string, raw syscall.RawConn) error {
		iface, err := net.InterfaceByName(interfaceName)
		if err != nil {
			return &InterfaceUnavailableError{Interface: interfaceName, Cause: err}
		}
		if iface.Flags&net.FlagUp == 0 {
			return &InterfaceUnavailableError{Interface: interfaceName, Cause: fmt.Errorf("interface is down")}
		}

		var socketErr error
		controlErr := raw.Control(func(fd uintptr) {
			switch network {
			case "tcp4", "udp4", "ip4":
				socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
			case "tcp6", "udp6", "ip6":
				socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, iface.Index)
			default:
				socketErr = fmt.Errorf("network %q has no deterministic address family", network)
			}
		})
		if controlErr != nil {
			return fmt.Errorf("control socket for interface %q: %w", interfaceName, controlErr)
		}
		if socketErr != nil {
			return fmt.Errorf("bind %s socket to interface %q: %w", network, interfaceName, socketErr)
		}
		return nil
	}
}
