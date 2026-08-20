//go:build darwin

package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Dialer is the runtime connection abstraction consumed by TCP sessions.
type Dialer interface {
	DialContext(ctx context.Context, network string, destination netip.AddrPort) (net.Conn, error)
}

type Direct struct {
	name           string
	interfaceName  string
	connectTimeout time.Duration
}

func NewDirect(name, interfaceName string, connectTimeout time.Duration) (*Direct, error) {
	if name == "" {
		return nil, errors.New("direct outbound name is required")
	}
	if interfaceName == "" {
		return nil, errors.New("direct outbound interface is required")
	}
	if connectTimeout <= 0 {
		return nil, errors.New("direct outbound connect timeout must be positive")
	}
	return &Direct{name: name, interfaceName: interfaceName, connectTimeout: connectTimeout}, nil
}

func (direct *Direct) DialContext(ctx context.Context, network string, destination netip.AddrPort) (net.Conn, error) {
	if !destination.IsValid() || destination.Port() == 0 || destination.Addr().Is4In6() {
		return nil, fmt.Errorf("direct outbound %q received invalid IP destination %s", direct.name, destination)
	}
	wantNetwork := "tcp4"
	if destination.Addr().Is6() {
		wantNetwork = "tcp6"
	}
	if network != wantNetwork {
		return nil, fmt.Errorf("direct outbound %q destination %s requires %q, got %q", direct.name, destination, wantNetwork, network)
	}
	dialer := net.Dialer{
		Timeout: direct.connectTimeout,
		Control: InterfaceControl(direct.interfaceName),
	}
	conn, err := dialer.DialContext(ctx, network, destination.String())
	if err != nil {
		return nil, fmt.Errorf("direct outbound %q via %s to %s: %w", direct.name, direct.interfaceName, destination, err)
	}
	return conn, nil
}

// DialPacket creates a connected UDP socket bound to this outbound's physical
// interface. A connected socket preserves one destination per UDP session.
func (direct *Direct) DialPacket(ctx context.Context, destination netip.AddrPort) (net.Conn, error) {
	if !destination.IsValid() || destination.Port() == 0 || destination.Addr().Is4In6() {
		return nil, fmt.Errorf("direct outbound %q received invalid IP UDP destination %s", direct.name, destination)
	}
	network := "udp4"
	if destination.Addr().Is6() {
		network = "udp6"
	}
	dialer := net.Dialer{
		Timeout: direct.connectTimeout,
		Control: InterfaceControl(direct.interfaceName),
	}
	conn, err := dialer.DialContext(ctx, network, destination.String())
	if err != nil {
		return nil, fmt.Errorf("direct outbound %q UDP via %s to %s: %w", direct.name, direct.interfaceName, destination, err)
	}
	return conn, nil
}

type Reject struct{}

func (Reject) DialContext(context.Context, string, netip.AddrPort) (net.Conn, error) {
	return nil, ErrRejected
}
