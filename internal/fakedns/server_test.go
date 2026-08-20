package fakedns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/domainname"
	"github.com/hailinpan/tun-proxy/internal/fakeip"
	"github.com/miekg/dns"
)

func TestServerUDPAndTCPBehavior(t *testing.T) {
	listen := availableAddress(t)
	pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := domainname.ParsePattern("*.local")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded atomic.Uint64
	upstream := ExchangeFunc(func(_ context.Context, request *dns.Msg) (*dns.Msg, error) {
		forwarded.Add(1)
		reply := new(dns.Msg)
		reply.SetReply(request)
		if request.Question[0].Qtype == dns.TypeA {
			reply.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.IPv4(192, 0, 2, 10),
			}}
		}
		return reply, nil
	})
	server, err := New(Config{Listen: listen, UDP: true, TCP: true, TTL: time.Minute, MaxConcurrent: 8}, pool, []domainname.Pattern{pattern}, upstream)
	if err != nil {
		t.Fatal(err)
	}
	running, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := running.Close(ctx); err != nil {
			t.Error(err)
		}
	})

	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			client := &dns.Client{Net: network, Timeout: time.Second}
			first := exchange(t, client, listen.String(), "example.com.", dns.TypeA)
			second := exchange(t, client, listen.String(), "other.example.", dns.TypeA)
			if len(first.Answer) != 1 || len(second.Answer) != 1 || first.Answer[0].String() == second.Answer[0].String() {
				t.Fatalf("Fake A answers are not distinct: %v %v", first.Answer, second.Answer)
			}
			aaaa := exchange(t, client, listen.String(), "example.com.", dns.TypeAAAA)
			if aaaa.Rcode != dns.RcodeSuccess || len(aaaa.Answer) != 0 {
				t.Fatalf("AAAA response = rcode %d answers %v", aaaa.Rcode, aaaa.Answer)
			}
			excluded := exchange(t, client, listen.String(), "printer.local.", dns.TypeA)
			if len(excluded.Answer) != 1 || !excluded.Answer[0].(*dns.A).A.Equal(net.IPv4(192, 0, 2, 10)) {
				t.Fatalf("excluded response = %v", excluded.Answer)
			}
			_ = exchange(t, client, listen.String(), "example.com.", dns.TypeTXT)
		})
	}
	if forwarded.Load() != 4 {
		t.Fatalf("forwarded queries = %d, want 4", forwarded.Load())
	}
	stats := server.Stats()
	if stats.FakeAnswers != 4 || stats.NODATAAnswers != 2 || stats.Forwarded != 4 || stats.Failures != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestServerStartsWithInheritedListeners(t *testing.T) {
	tcpListener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	listen := tcpListener.Addr().(*net.TCPAddr).AddrPort()
	udpConnection, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(listen))
	if err != nil {
		_ = tcpListener.Close()
		t.Fatal(err)
	}
	pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	upstream := ExchangeFunc(func(_ context.Context, request *dns.Msg) (*dns.Msg, error) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		return reply, nil
	})
	server, err := New(Config{
		Listen: listen, UDP: true, TCP: true, TTL: time.Minute, MaxConcurrent: 8,
	}, pool, nil, upstream)
	if err != nil {
		t.Fatal(err)
	}
	running, err := server.StartWithListeners(Listeners{UDP: udpConnection, TCP: tcpListener})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Close(t.Context()) })

	for _, network := range []string{"udp", "tcp"} {
		reply := exchange(t, &dns.Client{Net: network, Timeout: time.Second}, listen.String(), "inherited.example.", dns.TypeA)
		if len(reply.Answer) != 1 || !reply.Answer[0].(*dns.A).A.Equal(net.IPv4(198, 18, 0, 10)) {
			t.Fatalf("%s inherited response = %v", network, reply.Answer)
		}
	}
}

func TestRunningReportsServingLoopFailure(t *testing.T) {
	listener := &controlledFailListener{
		addr:    &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5353},
		entered: make(chan struct{}),
		fail:    make(chan struct{}),
	}
	pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Listen: netip.MustParseAddrPort("127.0.0.1:5353"), TCP: true, TTL: time.Minute, MaxConcurrent: 8,
	}, pool, nil, ExchangeFunc(func(context.Context, *dns.Msg) (*dns.Msg, error) { return new(dns.Msg), nil }))
	if err != nil {
		t.Fatal(err)
	}
	running, err := server.StartWithListeners(Listeners{TCP: listener})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-listener.entered:
	case <-time.After(time.Second):
		t.Fatal("Fake DNS did not enter the TCP serving loop")
	}
	close(listener.fail)
	select {
	case <-running.Done():
	case <-time.After(time.Second):
		t.Fatal("Fake DNS serving-loop failure was not reported")
	}
	if err := running.Err(); err == nil || !strings.Contains(err.Error(), "accept failed") {
		t.Fatalf("Running.Err() = %v, want accept failure", err)
	}
}

type controlledFailListener struct {
	addr      net.Addr
	entered   chan struct{}
	fail      chan struct{}
	enterOnce sync.Once
}

func (listener *controlledFailListener) Accept() (net.Conn, error) {
	listener.enterOnce.Do(func() { close(listener.entered) })
	<-listener.fail
	return nil, errors.New("accept failed")
}

func (listener *controlledFailListener) Close() error   { return nil }
func (listener *controlledFailListener) Addr() net.Addr { return listener.addr }

func TestRunningCloseDoesNotWaitPastContext(t *testing.T) {
	running := &Running{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := running.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context cancellation", err)
	}
}

func TestServerRejectsInheritedListenerAtWrongAddressWithoutClosingIt(t *testing.T) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	actual := connection.LocalAddr().(*net.UDPAddr).AddrPort()
	wanted := netip.AddrPortFrom(actual.Addr(), actual.Port()+1)
	pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Listen: wanted, UDP: true, TTL: time.Minute, MaxConcurrent: 8,
	}, pool, nil, ExchangeFunc(func(context.Context, *dns.Msg) (*dns.Msg, error) { return new(dns.Msg), nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.StartWithListeners(Listeners{UDP: connection}); err == nil {
		t.Fatal("wrong inherited listener address was accepted")
	}
	if _, err := connection.WriteTo([]byte{0}, connection.LocalAddr()); err != nil {
		t.Fatalf("validation failure closed caller-owned listener: %v", err)
	}
}

func TestServerReloadAffectsNewQueries(t *testing.T) {
	listen := availableAddress(t)
	pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	upstream := ExchangeFunc(func(_ context.Context, request *dns.Msg) (*dns.Msg, error) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		reply.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.IPv4(192, 0, 2, 20),
		}}
		return reply, nil
	})
	server, err := New(Config{Listen: listen, UDP: true, TTL: time.Minute, MaxConcurrent: 8}, pool, nil, upstream)
	if err != nil {
		t.Fatal(err)
	}
	running, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Close(t.Context()) })
	client := &dns.Client{Net: "udp", Timeout: time.Second}
	before := exchange(t, client, listen.String(), "reload.example.", dns.TypeA)
	if !before.Answer[0].(*dns.A).A.Equal(net.IPv4(198, 18, 0, 10)) {
		t.Fatalf("before reload = %v", before.Answer)
	}
	pattern, err := domainname.ParsePattern("reload.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Reload(2*time.Minute, time.Second, []domainname.Pattern{pattern}, upstream); err != nil {
		t.Fatal(err)
	}
	after := exchange(t, client, listen.String(), "reload.example.", dns.TypeA)
	if !after.Answer[0].(*dns.A).A.Equal(net.IPv4(192, 0, 2, 20)) {
		t.Fatalf("after reload = %v", after.Answer)
	}
}

func TestServerDualStackReturnsStableFamilyMatchedAnswers(t *testing.T) {
	listen := availableAddress(t)
	ipv4Pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	ipv6Pool, err := fakeip.New(netip.MustParsePrefix("fd00:7::/120"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	upstream := ExchangeFunc(func(_ context.Context, request *dns.Msg) (*dns.Msg, error) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		return reply, nil
	})
	server, err := NewDualStack(
		Config{Listen: listen, UDP: true, TTL: time.Minute, MaxConcurrent: 8},
		ipv4Pool, ipv6Pool, nil, upstream,
	)
	if err != nil {
		t.Fatal(err)
	}
	running, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Close(t.Context()) })
	client := &dns.Client{Net: "udp", Timeout: time.Second}
	a := exchange(t, client, listen.String(), "dual.example.", dns.TypeA)
	aaaa := exchange(t, client, listen.String(), "dual.example.", dns.TypeAAAA)
	if len(a.Answer) != 1 || !a.Answer[0].(*dns.A).A.Equal(net.IPv4(198, 18, 0, 10)) {
		t.Fatalf("A answer = %v", a.Answer)
	}
	wantIPv6 := net.ParseIP("fd00:7::a")
	if len(aaaa.Answer) != 1 || !aaaa.Answer[0].(*dns.AAAA).AAAA.Equal(wantIPv6) {
		t.Fatalf("AAAA answer = %v", aaaa.Answer)
	}
	stats := server.Stats()
	if stats.FakeAnswers != 2 || stats.FakeIPv4Answers != 1 || stats.FakeIPv6Answers != 1 || stats.NODATAAnswers != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestServerRejectsWrongPoolFamily(t *testing.T) {
	ipv4Pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	upstream := ExchangeFunc(func(context.Context, *dns.Msg) (*dns.Msg, error) { return new(dns.Msg), nil })
	_, err = NewDualStack(
		Config{Listen: availableAddress(t), UDP: true, TTL: time.Minute, MaxConcurrent: 8},
		ipv4Pool, ipv4Pool, nil, upstream,
	)
	if err == nil {
		t.Fatal("IPv4 pool was accepted as Fake IPv6 pool")
	}
}

func exchange(t *testing.T, client *dns.Client, server, name string, questionType uint16) *dns.Msg {
	t.Helper()
	request := new(dns.Msg)
	request.SetQuestion(name, questionType)
	reply, _, err := client.Exchange(request, server)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func availableAddress(t *testing.T) netip.AddrPort {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
}
