package session

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/rules"
)

type fakePacketDialer struct {
	results []dialResult
	calls   []netip.AddrPort
}

func (dialer *fakePacketDialer) DialPacket(_ context.Context, destination netip.AddrPort) (net.Conn, error) {
	dialer.calls = append(dialer.calls, destination)
	result := dialer.results[0]
	dialer.results = dialer.results[1:]
	return result.conn, result.err
}

func TestUDPHandleRelaysFirstDatagramAndExpires(t *testing.T) {
	pool := &fakePool{domain: "echo.example"}
	matcher, err := rules.New([]config.Rule{{ID: 1, Protocol: "udp", Outbound: "primary"}, {ID: 2, Outbound: "reject"}})
	if err != nil {
		t.Fatal(err)
	}
	upstream, server := net.Pipe()
	defer server.Close() //nolint:errcheck // Best-effort cleanup.
	packetDialer := &fakePacketDialer{results: []dialResult{{conn: upstream}}}
	udpSession, err := NewUDP(pool, matcher, map[string]Route{
		"primary": {
			Name: "primary", Resolver: &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("192.0.2.10")}},
			PacketDialer: packetDialer,
		},
		"reject": {Name: "reject", Reject: true},
	}, UDPConfig{IdleTimeout: 50 * time.Millisecond, MaxSessions: 4, MaxPerSource: 2})
	if err != nil {
		t.Fatal(err)
	}
	client, application := net.Pipe()
	defer application.Close() //nolint:errcheck // Best-effort cleanup.
	requests := [][]byte{[]byte("first UDP request"), []byte("second UDP request")}
	responses := [][]byte{[]byte("first UDP response"), []byte("second UDP response")}
	serverDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 128)
		for index := range requests {
			n, err := server.Read(buffer)
			if err != nil {
				serverDone <- err
				return
			}
			if string(buffer[:n]) != string(requests[index]) {
				serverDone <- errors.New("upstream request differs")
				return
			}
			if _, err := server.Write(responses[index]); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	handleDone := make(chan error, 1)
	go func() {
		handleDone <- udpSession.Handle(t.Context(), netstack.UDPFlow{
			SourceIP: netip.MustParseAddr("10.0.0.2"), SourcePort: 12345,
			DestinationIP: netip.MustParseAddr("198.18.0.10"), DestinationPort: 7000,
		}, client)
	}()
	got := make([]byte, 128)
	for index := range requests {
		if _, err := application.Write(requests[index]); err != nil {
			t.Fatal(err)
		}
		n, err := application.Read(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(got[:n]) != string(responses[index]) {
			t.Fatalf("response = %q, want %q", got[:n], responses[index])
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-handleDone:
		if err != nil {
			t.Fatalf("Handle = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("UDP session did not expire")
	}
	if pool.releases != 1 {
		t.Fatalf("Fake IP releases = %d, want 1", pool.releases)
	}
	stats := udpSession.Stats()
	if stats.TotalSessions != 1 || stats.ActiveSessions != 0 || stats.ExpiredSessions != 1 || stats.DatagramsToNetwork != 2 || stats.DatagramsToClient != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestUDPConnectFallsBackAfterRecoverableResolverFailure(t *testing.T) {
	matcher, err := rules.New([]config.Rule{{ID: 1, Outbound: "primary"}})
	if err != nil {
		t.Fatal(err)
	}
	upstream, server := net.Pipe()
	defer server.Close() //nolint:errcheck // Best-effort cleanup.
	fallbackDialer := &fakePacketDialer{results: []dialResult{{conn: upstream}}}
	udpSession, err := NewUDP(&fakePool{domain: "example.com"}, matcher, map[string]Route{
		"primary": {
			Name: "primary", Resolver: &fakeResolver{err: context.DeadlineExceeded},
			PacketDialer: &fakePacketDialer{}, Fallback: "fallback",
		},
		"fallback": {
			Name: "fallback", Resolver: &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.8")}},
			PacketDialer: fallbackDialer,
		},
	}, UDPConfig{IdleTimeout: time.Second, MaxSessions: 4, MaxPerSource: 2})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 16)
		_, err := io.ReadFull(server, buffer[:4])
		readDone <- err
	}()
	conn, selected, err := udpSession.connectAndSend(t.Context(), "primary", "example.com", netip.Addr{}, 443, false, []byte("QUIC"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // Best-effort cleanup.
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if selected != "fallback" || len(fallbackDialer.calls) != 1 || udpSession.Stats().FallbackAttempts != 1 {
		t.Fatalf("selected=%q calls=%v stats=%+v", selected, fallbackDialer.calls, udpSession.Stats())
	}
}

func TestUDPConnectIPv6UsesAAAAAddress(t *testing.T) {
	matcher, err := rules.New([]config.Rule{{ID: 1, Outbound: "primary"}})
	if err != nil {
		t.Fatal(err)
	}
	upstream, server := net.Pipe()
	defer server.Close() //nolint:errcheck // Best-effort cleanup.
	dialer := &fakePacketDialer{results: []dialResult{{conn: upstream}}}
	resolver := &fakeResolver{ipv6Addresses: []netip.Addr{netip.MustParseAddr("2001:db8::53")}}
	udpSession, err := NewUDP(&fakePool{domain: "example.com"}, matcher, map[string]Route{
		"primary": {Name: "primary", Resolver: resolver, PacketDialer: dialer},
	}, UDPConfig{IdleTimeout: time.Second, MaxSessions: 4, MaxPerSource: 2})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4)
		_, err := io.ReadFull(server, buffer)
		readDone <- err
	}()
	conn, selected, err := udpSession.connectAndSend(t.Context(), "primary", "example.com", netip.Addr{}, 443, true, []byte("QUIC"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // Best-effort cleanup.
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if selected != "primary" || resolver.calls != 0 || resolver.ipv6Calls != 1 || len(dialer.calls) != 1 || dialer.calls[0].String() != "[2001:db8::53]:443" {
		t.Fatalf("selected=%q resolver=%+v calls=%v", selected, resolver, dialer.calls)
	}
}

func TestUDPConnectLiteralSkipsResolver(t *testing.T) {
	matcher, err := rules.New([]config.Rule{{ID: 1, Outbound: "primary"}})
	if err != nil {
		t.Fatal(err)
	}
	upstream, server := net.Pipe()
	defer server.Close() //nolint:errcheck // Best-effort cleanup.
	dialer := &fakePacketDialer{results: []dialResult{{conn: upstream}}}
	resolver := &fakeResolver{err: errors.New("must not resolve a literal")}
	udpSession, err := NewUDP(&fakePool{domain: "example.com"}, matcher, map[string]Route{
		"primary": {Name: "primary", Resolver: resolver, PacketDialer: dialer},
	}, UDPConfig{IdleTimeout: time.Second, MaxSessions: 4, MaxPerSource: 2})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4)
		_, err := io.ReadFull(server, buffer)
		readDone <- err
	}()
	literal := netip.MustParseAddr("203.0.113.9")
	conn, selected, err := udpSession.connectAndSend(t.Context(), "primary", "", literal, 443, false, []byte("PING"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // Best-effort cleanup.
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if selected != "primary" || resolver.calls != 0 || len(dialer.calls) != 1 || dialer.calls[0] != netip.MustParseAddrPort("203.0.113.9:443") {
		t.Fatalf("selected=%q resolver_calls=%d dial_calls=%v", selected, resolver.calls, dialer.calls)
	}
}

func TestUDPSessionCapacityAndPerSourceLimit(t *testing.T) {
	matcher, err := rules.New([]config.Rule{{ID: 1, Outbound: "reject"}})
	if err != nil {
		t.Fatal(err)
	}
	udpSession, err := NewUDP(&fakePool{domain: "example.com"}, matcher, map[string]Route{
		"reject": {Name: "reject", Reject: true},
	}, UDPConfig{IdleTimeout: time.Second, MaxSessions: 2, MaxPerSource: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := UDPKey{SourceIP: netip.MustParseAddr("10.0.0.1"), SourcePort: 1, DestinationIP: netip.MustParseAddr("198.18.0.10"), DestinationPort: 53}
	if err := udpSession.register(first); err != nil {
		t.Fatal(err)
	}
	sameSource := first
	sameSource.SourcePort = 2
	if err := udpSession.register(sameSource); !errors.Is(err, ErrUDPCapacity) {
		t.Fatalf("same-source register = %v", err)
	}
	second := first
	second.SourceIP = netip.MustParseAddr("10.0.0.2")
	if err := udpSession.register(second); err != nil {
		t.Fatal(err)
	}
	third := first
	third.SourceIP = netip.MustParseAddr("10.0.0.3")
	if err := udpSession.register(third); !errors.Is(err, ErrUDPCapacity) {
		t.Fatalf("over-capacity register = %v", err)
	}
	udpSession.unregister(first)
	udpSession.unregister(second)
}

func TestUDPLimiterIsSharedAcrossPolicyGenerations(t *testing.T) {
	matcher, err := rules.New([]config.Rule{{ID: 1, Outbound: "reject"}})
	if err != nil {
		t.Fatal(err)
	}
	config := UDPConfig{IdleTimeout: time.Second, MaxSessions: 2, MaxPerSource: 1}
	limiter, err := NewUDPLimiter(config.MaxSessions, config.MaxPerSource)
	if err != nil {
		t.Fatal(err)
	}
	newGeneration := func() *UDP {
		generation, err := NewUDPWithLimiter(&fakePool{domain: "example.com"}, matcher, map[string]Route{
			"reject": {Name: "reject", Reject: true},
		}, config, limiter)
		if err != nil {
			t.Fatal(err)
		}
		return generation
	}
	firstGeneration := newGeneration()
	secondGeneration := newGeneration()

	first := UDPKey{SourceIP: netip.MustParseAddr("10.0.0.1"), SourcePort: 1, DestinationIP: netip.MustParseAddr("198.18.0.10"), DestinationPort: 53}
	if err := firstGeneration.register(first); err != nil {
		t.Fatal(err)
	}
	sameSource := first
	sameSource.SourcePort++
	if err := secondGeneration.register(sameSource); !errors.Is(err, ErrUDPCapacity) {
		t.Fatalf("cross-generation per-source register = %v", err)
	}
	second := first
	second.SourceIP = netip.MustParseAddr("10.0.0.2")
	if err := secondGeneration.register(second); err != nil {
		t.Fatal(err)
	}
	third := first
	third.SourceIP = netip.MustParseAddr("10.0.0.3")
	if err := firstGeneration.register(third); !errors.Is(err, ErrUDPCapacity) {
		t.Fatalf("cross-generation global register = %v", err)
	}

	firstGeneration.unregister(first)
	secondGeneration.unregister(second)
}
