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
	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/hailinpan/tun-proxy/internal/rules"
)

func TestDecideFlowReResolvesWhenCIDRChangesOutbound(t *testing.T) {
	matcher := newCIDRMatcher(t, "203.0.113.0/24", "special", "candidate")
	candidateResolver := &fakeResolver{addresses: []netip.Addr{
		netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("203.0.113.10"),
	}}
	specialResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("198.51.100.20")}}
	connected, peer := net.Pipe()
	defer peer.Close()
	specialDialer := &fakeDialer{results: []dialResult{{conn: connected}}}
	routes := map[string]Route{
		"candidate": {Name: "candidate", Resolver: candidateResolver, Dialer: &fakeDialer{}},
		"special":   {Name: "special", Resolver: specialResolver, Dialer: specialDialer},
	}
	metadata := rules.FlowMetadata{Domain: "example.com", Protocol: "tcp", DestinationPort: 443}
	decision, prepared, err := decideFlow(t.Context(), matcher, routes, metadata, "example.com", netip.Addr{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision != (rules.Decision{RuleID: 1, Outbound: "special"}) {
		t.Fatalf("decision = %+v", decision)
	}
	if candidateResolver.calls != 1 || specialResolver.calls != 1 || prepared == nil || prepared.Outbound != "special" {
		t.Fatalf("candidate calls=%d special calls=%d prepared=%+v", candidateResolver.calls, specialResolver.calls, prepared)
	}

	tcpSession := newTestTCP(t, routes)
	conn, selected, err := tcpSession.connectPrepared(t.Context(), decision.Outbound, "example.com", netip.Addr{}, 443, false, prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if selected != "special" || specialResolver.calls != 1 || len(specialDialer.calls) != 1 || specialDialer.calls[0] != netip.MustParseAddrPort("198.51.100.20:443") {
		t.Fatalf("selected=%q resolver calls=%d dial calls=%v", selected, specialResolver.calls, specialDialer.calls)
	}
}

func TestDecideFlowReusesResolverFallbackSelectedByCIDR(t *testing.T) {
	matcher := newCIDRMatcher(t, "203.0.113.0/24", "fallback", "candidate")
	candidateResolver := &fakeResolver{err: context.DeadlineExceeded}
	fallbackResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.8")}}
	routes := map[string]Route{
		"candidate": {Name: "candidate", Resolver: candidateResolver, Dialer: &fakeDialer{}, Fallback: "fallback"},
		"fallback":  {Name: "fallback", Resolver: fallbackResolver, Dialer: &fakeDialer{}},
	}
	fallbacks := 0
	decision, prepared, err := decideFlow(
		t.Context(), matcher, routes,
		rules.FlowMetadata{Domain: "example.com", Protocol: "tcp"},
		"example.com", netip.Addr{}, false, func() { fallbacks++ },
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outbound != "fallback" || prepared == nil || prepared.Outbound != "fallback" || fallbacks != 1 {
		t.Fatalf("decision=%+v prepared=%+v fallbacks=%d", decision, prepared, fallbacks)
	}
	if candidateResolver.calls != 1 || fallbackResolver.calls != 1 {
		t.Fatalf("resolver calls candidate=%d fallback=%d", candidateResolver.calls, fallbackResolver.calls)
	}
}

func TestDecideFlowMatchesLiteralWithoutResolution(t *testing.T) {
	matcher := newCIDRMatcher(t, "203.0.113.0/24", "special", "candidate")
	resolver := &fakeResolver{err: errors.New("must not resolve a literal")}
	connected, peer := net.Pipe()
	defer peer.Close()
	dialer := &fakeDialer{results: []dialResult{{conn: connected}}}
	routes := map[string]Route{
		"candidate": {Name: "candidate", Resolver: resolver, Dialer: &fakeDialer{}},
		"special":   {Name: "special", Resolver: resolver, Dialer: dialer},
	}
	literal := netip.MustParseAddr("203.0.113.9")
	metadata := rules.FlowMetadata{DestinationIP: literal, Protocol: "tcp", DestinationPort: 443}
	decision, prepared, err := decideFlow(t.Context(), matcher, routes, metadata, "", literal, false, nil)
	if err != nil || decision.Outbound != "special" || prepared != nil || resolver.calls != 0 {
		t.Fatalf("decision=%+v prepared=%+v resolver calls=%d err=%v", decision, prepared, resolver.calls, err)
	}
	tcpSession := newTestTCP(t, routes)
	conn, selected, err := tcpSession.connectPrepared(t.Context(), decision.Outbound, "", literal, 443, false, prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if selected != "special" || resolver.calls != 0 || len(dialer.calls) != 1 || dialer.calls[0] != netip.MustParseAddrPort("203.0.113.9:443") {
		t.Fatalf("selected=%q resolver calls=%d dial calls=%v", selected, resolver.calls, dialer.calls)
	}
}

func TestUDPUsesReResolvedAddressesWithoutThirdLookup(t *testing.T) {
	matcher := newCIDRMatcher(t, "203.0.113.0/24", "special", "candidate")
	candidateResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.53")}}
	specialResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("198.51.100.53")}}
	upstream, server := net.Pipe()
	defer server.Close()
	dialer := &fakePacketDialer{results: []dialResult{{conn: upstream}}}
	routes := map[string]Route{
		"candidate": {Name: "candidate", Resolver: candidateResolver, PacketDialer: &fakePacketDialer{}},
		"special":   {Name: "special", Resolver: specialResolver, PacketDialer: dialer},
	}
	metadata := rules.FlowMetadata{Domain: "example.com", Protocol: "udp", DestinationPort: 53}
	decision, prepared, err := decideFlow(t.Context(), matcher, routes, metadata, "example.com", netip.Addr{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	udpSession, err := NewUDP(&fakePool{domain: "example.com"}, matcher, routes, UDPConfig{
		IdleTimeout: time.Second, MaxSessions: 4, MaxPerSource: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4)
		_, err := io.ReadFull(server, buffer)
		readDone <- err
	}()
	conn, selected, err := udpSession.connectAndSendPrepared(t.Context(), decision.Outbound, "example.com", netip.Addr{}, 53, false, []byte("PING"), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if selected != "special" || specialResolver.calls != 1 || len(dialer.calls) != 1 || dialer.calls[0] != netip.MustParseAddrPort("198.51.100.53:53") {
		t.Fatalf("selected=%q resolver calls=%d dial calls=%v", selected, specialResolver.calls, dialer.calls)
	}
}

func TestDecideFlowCanFreezeRejectDecision(t *testing.T) {
	matcher := newCIDRMatcher(t, "203.0.113.0/24", "reject", "candidate")
	routes := map[string]Route{
		"candidate": {Name: "candidate", Resolver: &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}, Dialer: &fakeDialer{}},
		"reject":    {Name: "reject", Reject: true},
	}
	decision, prepared, err := decideFlow(
		t.Context(), matcher, routes,
		rules.FlowMetadata{Domain: "example.com", Protocol: "tcp"},
		"example.com", netip.Addr{}, false, nil,
	)
	if err != nil || decision.Outbound != "reject" || prepared != nil {
		t.Fatalf("decision=%+v prepared=%+v err=%v", decision, prepared, err)
	}
	tcpSession := newTestTCP(t, routes)
	_, _, err = tcpSession.connectPrepared(t.Context(), decision.Outbound, "example.com", netip.Addr{}, 443, false, prepared)
	if !errors.Is(err, outbound.ErrRejected) {
		t.Fatalf("connect error = %v", err)
	}
}

func TestDecideFlowUsesDeferredDirectCandidateBeforeDefaultReject(t *testing.T) {
	matcher := newCIDRMatcher(t, "203.0.113.0/24", "special", "reject")
	specialResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}
	routes := map[string]Route{
		"special": {Name: "special", Resolver: specialResolver, Dialer: &fakeDialer{}},
		"reject":  {Name: "reject", Reject: true},
	}
	decision, prepared, err := decideFlow(
		t.Context(), matcher, routes,
		rules.FlowMetadata{Domain: "example.com", Protocol: "tcp"},
		"example.com", netip.Addr{}, false, nil,
	)
	if err != nil || decision.Outbound != "special" || prepared == nil || prepared.Outbound != "special" || specialResolver.calls != 1 {
		t.Fatalf("decision=%+v prepared=%+v resolver calls=%d err=%v", decision, prepared, specialResolver.calls, err)
	}
}

func TestDecideFlowResolvesThroughDefaultBeforeCIDRReject(t *testing.T) {
	matcher := newCIDRMatcher(t, "203.0.113.0/24", "reject", "candidate")
	candidateResolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}
	routes := map[string]Route{
		"candidate": {Name: "candidate", Resolver: candidateResolver, Dialer: &fakeDialer{}},
		"reject":    {Name: "reject", Reject: true},
	}
	decision, prepared, err := decideFlow(
		t.Context(), matcher, routes,
		rules.FlowMetadata{Domain: "example.com", Protocol: "tcp"},
		"example.com", netip.Addr{}, false, nil,
	)
	if err != nil || decision.Outbound != "reject" || prepared != nil || candidateResolver.calls != 1 {
		t.Fatalf("decision=%+v prepared=%+v resolver calls=%d err=%v", decision, prepared, candidateResolver.calls, err)
	}
}

func newCIDRMatcher(t *testing.T, prefix, matchedOutbound, candidateOutbound string) *rules.Engine {
	t.Helper()
	matcher, err := rules.New([]config.Rule{
		{ID: 1, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix(prefix)}, Outbound: matchedOutbound},
		{ID: 2, Outbound: candidateOutbound},
	})
	if err != nil {
		t.Fatal(err)
	}
	return matcher
}
