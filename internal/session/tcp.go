// Package session coordinates flow lifetime without depending on gVisor
// implementation types.
package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/hailinpan/tun-proxy/internal/resolver"
	"github.com/hailinpan/tun-proxy/internal/rules"
)

type MappingPool interface {
	Acquire(address netip.Addr) (domain string, release func(), err error)
}

// MappingPools selects the independently persisted Fake IP pool that matches
// the intercepted destination address family.
type MappingPools struct {
	IPv4         MappingPool
	IPv6         MappingPool
	IPv4Prefix   netip.Prefix
	IPv6Prefix   netip.Prefix
	AllowLiteral bool
}

func (pools MappingPools) Acquire(address netip.Addr) (string, func(), error) {
	var pool MappingPool
	var prefix netip.Prefix
	switch {
	case address.Is4():
		pool = pools.IPv4
		prefix = pools.IPv4Prefix
	case address.Is6() && !address.Is4In6():
		pool = pools.IPv6
		prefix = pools.IPv6Prefix
	default:
		return "", nil, fmt.Errorf("unsupported Fake IP address family %s", address)
	}
	if pools.AllowLiteral && prefix.IsValid() && !prefix.Contains(address) {
		return "", func() {}, nil
	}
	if pool == nil {
		return "", nil, fmt.Errorf("Fake IP pool is not configured for %s", address)
	}
	return pool.Acquire(address)
}

type Matcher interface {
	Match(metadata rules.FlowMetadata) (rules.Decision, error)
	Candidates(metadata rules.FlowMetadata) ([]rules.Decision, error)
	MatchResolved(metadata rules.FlowMetadata, addresses []netip.Addr) (rules.Decision, error)
}

type IPResolver interface {
	LookupIPv4(ctx context.Context, domain string) ([]netip.Addr, error)
	LookupIPv6(ctx context.Context, domain string) ([]netip.Addr, error)
}

type Dialer interface {
	DialContext(ctx context.Context, network string, destination netip.AddrPort) (net.Conn, error)
}

type PacketDialer interface {
	DialPacket(ctx context.Context, destination netip.AddrPort) (net.Conn, error)
}

type Route struct {
	Name         string
	Reject       bool
	Resolver     IPResolver
	Dialer       Dialer
	PacketDialer PacketDialer
	Fallback     string
}

type Stats struct {
	TotalFlows       uint64
	ActiveFlows      uint64
	CompletedFlows   uint64
	FailedFlows      uint64
	FallbackAttempts uint64
	RejectedFlows    uint64
}

type sessionCounters struct {
	totalFlows       atomic.Uint64
	activeFlows      atomic.Uint64
	completedFlows   atomic.Uint64
	failedFlows      atomic.Uint64
	fallbackAttempts atomic.Uint64
	rejectedFlows    atomic.Uint64
}

type TCP struct {
	pool         MappingPool
	matcher      Matcher
	routes       map[string]Route
	relayTimeout time.Duration
	metrics      sessionCounters
}

func NewTCP(pool MappingPool, matcher Matcher, routes map[string]Route, relayTimeout time.Duration) (*TCP, error) {
	if pool == nil || matcher == nil {
		return nil, errors.New("TCP session requires a Fake IP pool and rules matcher")
	}
	if len(routes) == 0 {
		return nil, errors.New("TCP session requires at least one outbound route")
	}
	if relayTimeout <= 0 {
		return nil, errors.New("TCP session relay timeout must be positive")
	}
	copyRoutes := make(map[string]Route, len(routes))
	for name, route := range routes {
		if name == "" || route.Name != name {
			return nil, fmt.Errorf("outbound route key %q does not match name %q", name, route.Name)
		}
		if route.Reject {
			if route.Resolver != nil || route.Dialer != nil || route.Fallback != "" {
				return nil, fmt.Errorf("reject route %q cannot resolve, dial, or fallback", name)
			}
		} else if route.Resolver == nil || route.Dialer == nil {
			return nil, fmt.Errorf("direct route %q requires resolver and dialer", name)
		}
		copyRoutes[name] = route
	}
	if err := validateFallbacks(copyRoutes); err != nil {
		return nil, err
	}
	return &TCP{pool: pool, matcher: matcher, routes: copyRoutes, relayTimeout: relayTimeout}, nil
}

// Handle implements netstack.TCPHandler.
func (tcpSession *TCP) Handle(ctx context.Context, flow netstack.TCPFlow, client net.Conn) error {
	tcpSession.metrics.totalFlows.Add(1)
	tcpSession.metrics.activeFlows.Add(1)
	defer tcpSession.metrics.activeFlows.Add(^uint64(0))

	domain, release, err := tcpSession.pool.Acquire(flow.DestinationIP)
	if err != nil {
		tcpSession.metrics.failedFlows.Add(1)
		return fmt.Errorf("acquire Fake IP %s for TCP flow: %w", flow.DestinationIP, err)
	}
	defer release()

	metadata := rules.FlowMetadata{
		Domain:        domain,
		DestinationIP: flow.DestinationIP,
		SourceIP:      flow.SourceIP,
		SourcePort:    flow.SourcePort,
	}
	var literal netip.Addr
	if domain == "" {
		literal = flow.DestinationIP
	} else {
		metadata.FakeIP = flow.DestinationIP
	}
	decision, prepared, err := decideFlow(ctx, tcpSession.matcher, tcpSession.routes, metadata, domain, literal, flow.DestinationIP.Is6(), func() {
		tcpSession.metrics.fallbackAttempts.Add(1)
	})
	if err != nil {
		tcpSession.metrics.failedFlows.Add(1)
		if errors.Is(err, outbound.ErrRejected) {
			tcpSession.metrics.rejectedFlows.Add(1)
		}
		return fmt.Errorf("decide TCP flow for %s: %w", domain, err)
	}

	upstream, selected, err := tcpSession.connectPrepared(ctx, decision.Outbound, domain, literal, flow.DestinationPort, flow.DestinationIP.Is6(), prepared)
	if err != nil {
		tcpSession.metrics.failedFlows.Add(1)
		if errors.Is(err, outbound.ErrRejected) {
			tcpSession.metrics.rejectedFlows.Add(1)
		}
		return fmt.Errorf("rule %d outbound %q for %s: %w", decision.RuleID, decision.Outbound, domain, err)
	}
	defer upstream.Close() //nolint:errcheck // Best-effort cleanup.

	if err := netstack.Relay(ctx, client, upstream, tcpSession.relayTimeout); err != nil {
		tcpSession.metrics.failedFlows.Add(1)
		return fmt.Errorf("relay %s through outbound %q: %w", domain, selected, err)
	}
	tcpSession.metrics.completedFlows.Add(1)
	return nil
}

func (tcpSession *TCP) connect(ctx context.Context, outboundName, domain string, literal netip.Addr, port uint16, ipv6 bool) (net.Conn, string, error) {
	return tcpSession.connectPrepared(ctx, outboundName, domain, literal, port, ipv6, nil)
}

func (tcpSession *TCP) connectPrepared(ctx context.Context, outboundName, domain string, literal netip.Addr, port uint16, ipv6 bool, prepared *routeResolution) (net.Conn, string, error) {
	current := outboundName
	if prepared != nil {
		current = prepared.Outbound
	}
	var failures []error
	for current != "" {
		route, ok := tcpSession.routes[current]
		if !ok {
			return nil, "", fmt.Errorf("outbound %q is not configured", current)
		}
		if route.Reject {
			return nil, "", errors.Join(append(failures, fmt.Errorf("outbound %q: %w", current, outbound.ErrRejected))...)
		}

		addresses := []netip.Addr{literal}
		var err error
		if prepared != nil && prepared.Outbound == current {
			addresses = prepared.Addresses
			prepared = nil
		} else if !literal.IsValid() {
			addresses, err = lookupAddresses(ctx, route.Resolver, domain, ipv6)
		}
		if err != nil {
			failure := fmt.Errorf("outbound %q resolve %s: %w", current, domain, err)
			failures = append(failures, failure)
			if resolver.IsBusinessError(err) || !outbound.IsRecoverable(err) || route.Fallback == "" {
				return nil, "", errors.Join(failures...)
			}
			tcpSession.metrics.fallbackAttempts.Add(1)
			current = route.Fallback
			continue
		}

		allRecoverable := true
		network := "tcp4"
		if ipv6 {
			network = "tcp6"
		}
		for _, address := range addresses {
			destination := netip.AddrPortFrom(address, port)
			conn, err := route.Dialer.DialContext(ctx, network, destination)
			if err == nil {
				return conn, current, nil
			}
			failures = append(failures, fmt.Errorf("outbound %q dial %s: %w", current, destination, err))
			if !outbound.IsRecoverable(err) {
				allRecoverable = false
			}
		}
		if !allRecoverable || route.Fallback == "" {
			return nil, "", errors.Join(failures...)
		}
		tcpSession.metrics.fallbackAttempts.Add(1)
		current = route.Fallback
	}
	return nil, "", errors.Join(failures...)
}

func lookupAddresses(ctx context.Context, resolver IPResolver, domain string, ipv6 bool) ([]netip.Addr, error) {
	if ipv6 {
		return resolver.LookupIPv6(ctx, domain)
	}
	return resolver.LookupIPv4(ctx, domain)
}

func validateFallbacks(routes map[string]Route) error {
	states := make(map[string]uint8, len(routes))
	var visit func(string) error
	visit = func(name string) error {
		switch states[name] {
		case 1:
			return fmt.Errorf("outbound fallback cycle includes %q", name)
		case 2:
			return nil
		}
		states[name] = 1
		next := routes[name].Fallback
		if next != "" {
			if _, exists := routes[next]; !exists {
				return fmt.Errorf("outbound %q fallback %q is not configured", name, next)
			}
			if err := visit(next); err != nil {
				return err
			}
		}
		states[name] = 2
		return nil
	}
	for name := range routes {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func (tcpSession *TCP) Stats() Stats {
	return Stats{
		TotalFlows:       tcpSession.metrics.totalFlows.Load(),
		ActiveFlows:      tcpSession.metrics.activeFlows.Load(),
		CompletedFlows:   tcpSession.metrics.completedFlows.Load(),
		FailedFlows:      tcpSession.metrics.failedFlows.Load(),
		FallbackAttempts: tcpSession.metrics.fallbackAttempts.Load(),
		RejectedFlows:    tcpSession.metrics.rejectedFlows.Load(),
	}
}
