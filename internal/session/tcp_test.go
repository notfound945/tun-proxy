package session

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/hailinpan/tun-proxy/internal/resolver"
	"github.com/hailinpan/tun-proxy/internal/rules"
)

type fakePool struct {
	domain   string
	releases int
}

func (pool *fakePool) Acquire(netip.Addr) (string, func(), error) {
	return pool.domain, func() { pool.releases++ }, nil
}

type fakeResolver struct {
	addresses     []netip.Addr
	ipv6Addresses []netip.Addr
	err           error
	calls         int
	ipv6Calls     int
}

func (resolver *fakeResolver) LookupIPv6(context.Context, string) ([]netip.Addr, error) {
	resolver.ipv6Calls++
	return resolver.ipv6Addresses, resolver.err
}

func (resolver *fakeResolver) LookupIPv4(context.Context, string) ([]netip.Addr, error) {
	resolver.calls++
	return resolver.addresses, resolver.err
}

type dialResult struct {
	conn net.Conn
	err  error
}

type fakeDialer struct {
	results  []dialResult
	calls    []netip.AddrPort
	networks []string
}

func (dialer *fakeDialer) DialContext(_ context.Context, network string, destination netip.AddrPort) (net.Conn, error) {
	dialer.calls = append(dialer.calls, destination)
	dialer.networks = append(dialer.networks, network)
	result := dialer.results[0]
	dialer.results = dialer.results[1:]
	return result.conn, result.err
}

func TestConnectTriesAllAddressesThenRecoverableFallback(t *testing.T) {
	primaryResolver := &fakeResolver{addresses: []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	}}
	primaryDialer := &fakeDialer{results: []dialResult{{err: syscall.ENETUNREACH}, {err: context.DeadlineExceeded}}}
	fallbackResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.1")}}
	connected, peer := net.Pipe()
	defer peer.Close()
	fallbackDialer := &fakeDialer{results: []dialResult{{conn: connected}}}
	tcpSession := newTestTCP(t, map[string]Route{
		"primary":  {Name: "primary", Resolver: primaryResolver, Dialer: primaryDialer, Fallback: "fallback"},
		"fallback": {Name: "fallback", Resolver: fallbackResolver, Dialer: fallbackDialer},
	})

	conn, selected, err := tcpSession.connect(t.Context(), "primary", "example.com", netip.Addr{}, 443, false)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if selected != "fallback" || len(primaryDialer.calls) != 2 || len(fallbackDialer.calls) != 1 {
		t.Fatalf("selected=%q primary calls=%v fallback calls=%v", selected, primaryDialer.calls, fallbackDialer.calls)
	}
	if tcpSession.Stats().FallbackAttempts != 1 {
		t.Fatalf("stats = %+v", tcpSession.Stats())
	}
}

func TestConnectDoesNotFallbackOnNXDOMAIN(t *testing.T) {
	primaryResolver := &fakeResolver{err: &resolver.ResponseError{Domain: "missing.example", RCode: 3}}
	fallbackResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.1")}}
	fallbackDialer := &fakeDialer{results: []dialResult{{err: errors.New("must not be called")}}}
	tcpSession := newTestTCP(t, map[string]Route{
		"primary":  {Name: "primary", Resolver: primaryResolver, Dialer: &fakeDialer{}, Fallback: "fallback"},
		"fallback": {Name: "fallback", Resolver: fallbackResolver, Dialer: fallbackDialer},
	})
	_, _, err := tcpSession.connect(t.Context(), "primary", "missing.example", netip.Addr{}, 443, false)
	if err == nil || fallbackResolver.calls != 0 || len(fallbackDialer.calls) != 0 {
		t.Fatalf("error=%v fallback resolver calls=%d dial calls=%d", err, fallbackResolver.calls, len(fallbackDialer.calls))
	}
}

func TestConnectDoesNotFallbackOnConnectionRefused(t *testing.T) {
	primaryDialer := &fakeDialer{results: []dialResult{{err: syscall.ECONNREFUSED}}}
	fallbackResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.1")}}
	tcpSession := newTestTCP(t, map[string]Route{
		"primary": {
			Name: "primary", Resolver: &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("192.0.2.1")}},
			Dialer: primaryDialer, Fallback: "fallback",
		},
		"fallback": {Name: "fallback", Resolver: fallbackResolver, Dialer: &fakeDialer{}},
	})
	_, _, err := tcpSession.connect(t.Context(), "primary", "example.com", netip.Addr{}, 443, false)
	if err == nil || fallbackResolver.calls != 0 || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("error=%v fallback calls=%d", err, fallbackResolver.calls)
	}
}

func TestConnectIPv6UsesAAAAAndTCP6(t *testing.T) {
	resolver := &fakeResolver{ipv6Addresses: []netip.Addr{netip.MustParseAddr("2001:db8::10")}}
	connected, peer := net.Pipe()
	defer peer.Close()
	dialer := &fakeDialer{results: []dialResult{{conn: connected}}}
	tcpSession := newTestTCP(t, map[string]Route{
		"primary": {Name: "primary", Resolver: resolver, Dialer: dialer},
	})
	conn, selected, err := tcpSession.connect(t.Context(), "primary", "example.com", netip.Addr{}, 443, true)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if selected != "primary" || resolver.calls != 0 || resolver.ipv6Calls != 1 {
		t.Fatalf("selected=%q A calls=%d AAAA calls=%d", selected, resolver.calls, resolver.ipv6Calls)
	}
	if len(dialer.calls) != 1 || dialer.calls[0].String() != "[2001:db8::10]:443" || dialer.networks[0] != "tcp6" {
		t.Fatalf("dial calls=%v networks=%v", dialer.calls, dialer.networks)
	}
}

func TestConnectIPv6DoesNotFallbackWithoutAAAA(t *testing.T) {
	primaryResolver := &fakeResolver{err: resolver.ErrNoIPv6Address}
	fallbackResolver := &fakeResolver{ipv6Addresses: []netip.Addr{netip.MustParseAddr("2001:db8::20")}}
	tcpSession := newTestTCP(t, map[string]Route{
		"primary": {
			Name: "primary", Resolver: primaryResolver, Dialer: &fakeDialer{}, Fallback: "fallback",
		},
		"fallback": {Name: "fallback", Resolver: fallbackResolver, Dialer: &fakeDialer{}},
	})
	_, _, err := tcpSession.connect(t.Context(), "primary", "example.com", netip.Addr{}, 443, true)
	if !errors.Is(err, resolver.ErrNoIPv6Address) || fallbackResolver.ipv6Calls != 0 || tcpSession.Stats().FallbackAttempts != 0 {
		t.Fatalf("error=%v fallback AAAA calls=%d stats=%+v", err, fallbackResolver.ipv6Calls, tcpSession.Stats())
	}
}

func TestMappingPoolsSelectsDestinationFamily(t *testing.T) {
	ipv4 := &fakePool{domain: "v4.example"}
	ipv6 := &fakePool{domain: "v6.example"}
	pools := MappingPools{IPv4: ipv4, IPv6: ipv6}
	domain, release, err := pools.Acquire(netip.MustParseAddr("198.18.0.10"))
	if err != nil || domain != "v4.example" {
		t.Fatalf("IPv4 Acquire() = %q, %v", domain, err)
	}
	release()
	domain, release, err = pools.Acquire(netip.MustParseAddr("fd00:7::10"))
	if err != nil || domain != "v6.example" {
		t.Fatalf("IPv6 Acquire() = %q, %v", domain, err)
	}
	release()
	if ipv4.releases != 1 || ipv6.releases != 1 {
		t.Fatalf("releases IPv4=%d IPv6=%d", ipv4.releases, ipv6.releases)
	}
	_, _, err = (MappingPools{IPv4: ipv4}).Acquire(netip.MustParseAddr("fd00:7::10"))
	if err == nil {
		t.Fatal("missing IPv6 pool was accepted")
	}
}

func TestMappingPoolsAllowsLiteralOutsideFakePrefix(t *testing.T) {
	pool := &fakePool{domain: "must-not-be-used.example"}
	pools := MappingPools{
		IPv4: pool, IPv4Prefix: netip.MustParsePrefix("198.18.0.0/15"), AllowLiteral: true,
	}
	domain, release, err := pools.Acquire(netip.MustParseAddr("1.1.1.1"))
	if err != nil || domain != "" {
		t.Fatalf("Acquire literal = %q, %v", domain, err)
	}
	release()
	if pool.releases != 0 {
		t.Fatalf("literal used Fake IP pool: releases=%d", pool.releases)
	}
}

func TestConnectLiteralSkipsResolver(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("must not resolve a literal")}
	connected, peer := net.Pipe()
	defer peer.Close()
	dialer := &fakeDialer{results: []dialResult{{conn: connected}}}
	tcpSession := newTestTCP(t, map[string]Route{
		"primary": {Name: "primary", Resolver: resolver, Dialer: dialer},
	})
	literal := netip.MustParseAddr("203.0.113.9")
	conn, selected, err := tcpSession.connect(t.Context(), "primary", "", literal, 443, false)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if selected != "primary" || resolver.calls != 0 || len(dialer.calls) != 1 || dialer.calls[0] != netip.MustParseAddrPort("203.0.113.9:443") {
		t.Fatalf("selected=%q resolver_calls=%d dial_calls=%v", selected, resolver.calls, dialer.calls)
	}
}

func TestHandleRejectReleasesFakeIPReference(t *testing.T) {
	pool := &fakePool{domain: "blocked.example"}
	matcher, err := rules.New([]config.Rule{{ID: 1, Outbound: "reject"}})
	if err != nil {
		t.Fatal(err)
	}
	tcpSession, err := NewTCP(pool, matcher, map[string]Route{
		"reject": {Name: "reject", Reject: true},
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	err = tcpSession.Handle(t.Context(), testFlow(), client)
	if !errors.Is(err, outbound.ErrRejected) {
		t.Fatalf("Handle error = %v", err)
	}
	if pool.releases != 1 {
		t.Fatalf("Fake IP releases = %d, want 1", pool.releases)
	}
	stats := tcpSession.Stats()
	if stats.TotalFlows != 1 || stats.FailedFlows != 1 || stats.RejectedFlows != 1 || stats.ActiveFlows != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func newTestTCP(t *testing.T, routes map[string]Route) *TCP {
	t.Helper()
	matcher, err := rules.New([]config.Rule{{ID: 1, Outbound: "primary"}})
	if err != nil {
		t.Fatal(err)
	}
	tcpSession, err := NewTCP(&fakePool{domain: "example.com"}, matcher, routes, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return tcpSession
}

func testFlow() netstack.TCPFlow {
	return netstack.TCPFlow{
		SourceIP: netip.MustParseAddr("10.0.0.2"), SourcePort: 12345,
		DestinationIP: netip.MustParseAddr("198.18.0.10"), DestinationPort: 443,
	}
}
