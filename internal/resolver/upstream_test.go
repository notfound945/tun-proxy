package resolver

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestUDPTruncationFallsBackToTCPOnBoundInterface(t *testing.T) {
	address := availableDNSAddress(t)
	packetConnection, err := net.ListenPacket("udp4", address.String())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", address.String())
	if err != nil {
		packetConnection.Close()
		t.Fatal(err)
	}
	var udpQueries atomic.Uint64
	var tcpQueries atomic.Uint64
	handler := dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		if _, udp := writer.RemoteAddr().(*net.UDPAddr); udp {
			udpQueries.Add(1)
			reply.Truncated = true
		} else {
			tcpQueries.Add(1)
			reply.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.IPv4(192, 0, 2, 1),
			}}
		}
		_ = writer.WriteMsg(reply)
	})
	udpServer := &dns.Server{PacketConn: packetConnection, Handler: handler}
	tcpServer := &dns.Server{Listener: listener, Handler: handler}
	go udpServer.ActivateAndServe()
	go tcpServer.ActivateAndServe()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = udpServer.ShutdownContext(ctx)
		_ = tcpServer.ShutdownContext(ctx)
	})

	client, err := newClient("lo0", []netip.AddrPort{address}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	request := new(dns.Msg)
	request.SetQuestion("example.com.", dns.TypeA)
	reply, err := client.Exchange(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Answer) != 1 || udpQueries.Load() != 1 || tcpQueries.Load() != 1 {
		t.Fatalf("reply=%v udp=%d tcp=%d", reply.Answer, udpQueries.Load(), tcpQueries.Load())
	}
}

func TestIPv6UDPTruncationFallsBackToTCPOnBoundInterface(t *testing.T) {
	address := availableIPv6DNSAddress(t)
	packetConnection, err := net.ListenPacket("udp6", address.String())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp6", address.String())
	if err != nil {
		packetConnection.Close()
		t.Fatal(err)
	}
	var udpQueries atomic.Uint64
	var tcpQueries atomic.Uint64
	handler := dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		if _, udp := writer.RemoteAddr().(*net.UDPAddr); udp {
			udpQueries.Add(1)
			reply.Truncated = true
		} else {
			tcpQueries.Add(1)
			reply.Answer = []dns.RR{&dns.AAAA{
				Hdr:  dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60},
				AAAA: net.ParseIP("2001:db8::1"),
			}}
		}
		_ = writer.WriteMsg(reply)
	})
	udpServer := &dns.Server{PacketConn: packetConnection, Handler: handler}
	tcpServer := &dns.Server{Listener: listener, Handler: handler}
	go udpServer.ActivateAndServe()
	go tcpServer.ActivateAndServe()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = udpServer.ShutdownContext(ctx)
		_ = tcpServer.ShutdownContext(ctx)
	})

	client, err := newClient("lo0", []netip.AddrPort{address}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	request := new(dns.Msg)
	request.SetQuestion("example.com.", dns.TypeAAAA)
	reply, err := client.Exchange(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Answer) != 1 || udpQueries.Load() != 1 || tcpQueries.Load() != 1 {
		t.Fatalf("reply=%v udp=%d tcp=%d", reply.Answer, udpQueries.Load(), tcpQueries.Load())
	}
}

func TestSERVFAILTriesNextExplicitUpstream(t *testing.T) {
	var firstQueries atomic.Uint64
	firstAddress := startUDPDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		firstQueries.Add(1)
		reply := new(dns.Msg)
		reply.SetRcode(request, dns.RcodeServerFailure)
		_ = writer.WriteMsg(reply)
	}))
	var secondQueries atomic.Uint64
	secondAddress := startUDPDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		secondQueries.Add(1)
		reply := new(dns.Msg)
		reply.SetReply(request)
		reply.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.IPv4(192, 0, 2, 9),
		}}
		_ = writer.WriteMsg(reply)
	}))

	client, err := newClient("lo0", []netip.AddrPort{firstAddress, secondAddress}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := client.LookupIPv4(t.Context(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || addresses[0].String() != "192.0.2.9" {
		t.Fatalf("addresses = %v", addresses)
	}
	if firstQueries.Load() != 1 || secondQueries.Load() != 1 {
		t.Fatalf("queries first=%d second=%d, want 1 each", firstQueries.Load(), secondQueries.Load())
	}
}

func TestNXDOMAINDoesNotTryNextExplicitUpstream(t *testing.T) {
	var firstQueries atomic.Uint64
	firstAddress := startUDPDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		firstQueries.Add(1)
		reply := new(dns.Msg)
		reply.SetRcode(request, dns.RcodeNameError)
		_ = writer.WriteMsg(reply)
	}))
	var secondQueries atomic.Uint64
	secondAddress := startUDPDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		secondQueries.Add(1)
		reply := new(dns.Msg)
		reply.SetReply(request)
		_ = writer.WriteMsg(reply)
	}))

	client, err := newClient("lo0", []netip.AddrPort{firstAddress, secondAddress}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LookupIPv4(t.Context(), "missing.test")
	if err == nil || !IsBusinessError(err) {
		t.Fatalf("LookupIPv4 error = %v, want terminal business error", err)
	}
	if firstQueries.Load() != 1 || secondQueries.Load() != 0 {
		t.Fatalf("queries first=%d second=%d, want first=1 second=0", firstQueries.Load(), secondQueries.Load())
	}
}

func TestTemporaryDNSResponseIsNotBusinessError(t *testing.T) {
	for _, rcode := range []int{dns.RcodeServerFailure, dns.RcodeRefused, dns.RcodeNotImplemented} {
		if IsBusinessError(&ResponseError{Domain: "example.test", RCode: rcode}) {
			t.Fatalf("%s was classified as a business error", dns.RcodeToString[rcode])
		}
	}
}

func TestClientRejectsLoopbackUpstream(t *testing.T) {
	_, err := NewClient("en0", []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:53")}, time.Second, 1)
	if err == nil {
		t.Fatal("NewClient accepted a loopback upstream that could recurse into Fake DNS")
	}
}

func TestLookupIPv4CachesPerClient(t *testing.T) {
	address := availableDNSAddress(t)
	packetConnection, err := net.ListenPacket("udp4", address.String())
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Uint64
	server := &dns.Server{PacketConn: packetConnection, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		queries.Add(1)
		reply := new(dns.Msg)
		reply.SetReply(request)
		reply.Answer = []dns.RR{
			&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)},
			&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 2)},
		}
		_ = writer.WriteMsg(reply)
	})}
	go server.ActivateAndServe()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownContext(ctx)
	})

	client, err := newClient("lo0", []netip.AddrPort{address}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		addresses, err := client.LookupIPv4(t.Context(), "Example.COM.")
		if err != nil {
			t.Fatal(err)
		}
		if len(addresses) != 2 || addresses[0].String() != "192.0.2.1" || addresses[1].String() != "192.0.2.2" {
			t.Fatalf("addresses = %v", addresses)
		}
		addresses[0] = netip.MustParseAddr("203.0.113.1")
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("upstream queries = %d, want 1", got)
	}
}

func TestLookupIPv4AndIPv6CacheIndependently(t *testing.T) {
	address := availableDNSAddress(t)
	packetConnection, err := net.ListenPacket("udp4", address.String())
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Uint64
	server := &dns.Server{PacketConn: packetConnection, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		queries.Add(1)
		reply := new(dns.Msg)
		reply.SetReply(request)
		switch request.Question[0].Qtype {
		case dns.TypeA:
			reply.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.IPv4(192, 0, 2, 10),
			}}
		case dns.TypeAAAA:
			reply.Answer = []dns.RR{&dns.AAAA{
				Hdr:  dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60},
				AAAA: net.ParseIP("2001:db8::10"),
			}}
		}
		_ = writer.WriteMsg(reply)
	})}
	go server.ActivateAndServe()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownContext(ctx)
	})

	client, err := newClient("lo0", []netip.AddrPort{address}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ipv4Addresses, err := client.LookupIPv4(t.Context(), "dual.example")
		if err != nil || len(ipv4Addresses) != 1 || ipv4Addresses[0].String() != "192.0.2.10" {
			t.Fatalf("LookupIPv4() = %v, %v", ipv4Addresses, err)
		}
		ipv6Addresses, err := client.LookupIPv6(t.Context(), "dual.example")
		if err != nil || len(ipv6Addresses) != 1 || ipv6Addresses[0].String() != "2001:db8::10" {
			t.Fatalf("LookupIPv6() = %v, %v", ipv6Addresses, err)
		}
	}
	if got := queries.Load(); got != 2 {
		t.Fatalf("upstream queries = %d, want one A and one AAAA query", got)
	}
}

func TestLookupsUseCNAMEChainTTLAndIgnoreUnrelatedAddresses(t *testing.T) {
	tests := []struct {
		name       string
		qtype      uint16
		lookup     func(*Client, context.Context, string) ([]netip.Addr, error)
		addressFor func(uint64) dns.RR
		unrelated  dns.RR
		wantFirst  string
		wantSecond string
	}{
		{
			name:  "IPv4",
			qtype: dns.TypeA,
			lookup: func(client *Client, ctx context.Context, domain string) ([]netip.Addr, error) {
				return client.LookupIPv4(ctx, domain)
			},
			addressFor: func(query uint64) dns.RR {
				return &dns.A{
					Hdr: dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
					A:   net.IPv4(192, 0, 2, byte(query)),
				}
			},
			unrelated: &dns.A{
				Hdr: dns.RR_Header{Name: "unrelated.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
				A:   net.IPv4(203, 0, 113, 9),
			},
			wantFirst:  "192.0.2.1",
			wantSecond: "192.0.2.2",
		},
		{
			name:  "IPv6",
			qtype: dns.TypeAAAA,
			lookup: func(client *Client, ctx context.Context, domain string) ([]netip.Addr, error) {
				return client.LookupIPv6(ctx, domain)
			},
			addressFor: func(query uint64) dns.RR {
				return &dns.AAAA{
					Hdr:  dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 300},
					AAAA: net.ParseIP(fmt.Sprintf("2001:db8::%d", query)),
				}
			},
			unrelated: &dns.AAAA{
				Hdr:  dns.RR_Header{Name: "unrelated.example.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 300},
				AAAA: net.ParseIP("2001:db8:ffff::9"),
			},
			wantFirst:  "2001:db8::1",
			wantSecond: "2001:db8::2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address := availableDNSAddress(t)
			packetConnection, err := net.ListenPacket("udp4", address.String())
			if err != nil {
				t.Fatal(err)
			}
			var queries atomic.Uint64
			server := &dns.Server{PacketConn: packetConnection, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
				query := queries.Add(1)
				reply := new(dns.Msg)
				reply.SetReply(request)
				reply.Answer = []dns.RR{
					&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 1}, Target: "target.example."},
					test.unrelated,
					test.addressFor(query),
				}
				_ = writer.WriteMsg(reply)
			})}
			go server.ActivateAndServe()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = server.ShutdownContext(ctx)
			})

			client, err := newClient("lo0", []netip.AddrPort{address}, time.Second, 4, true)
			if err != nil {
				t.Fatal(err)
			}
			first, err := test.lookup(client, t.Context(), "alias.example")
			if err != nil {
				t.Fatal(err)
			}
			if len(first) != 1 || first[0].String() != test.wantFirst {
				t.Fatalf("first lookup = %v, want only %s", first, test.wantFirst)
			}

			// Advance the cache clock past the CNAME TTL without sleeping. If the
			// implementation incorrectly uses only the address TTL, this leaves
			// the old entry cached and the second upstream query never occurs.
			if cached := client.cached("alias.example", test.qtype, time.Now().Add(2*time.Second)); len(cached) != 0 {
				t.Fatalf("entry remained cached past CNAME TTL: %v", cached)
			}
			second, err := test.lookup(client, t.Context(), "alias.example")
			if err != nil {
				t.Fatal(err)
			}
			if len(second) != 1 || second[0].String() != test.wantSecond {
				t.Fatalf("second lookup = %v, want %s", second, test.wantSecond)
			}
			if got := queries.Load(); got != 2 {
				t.Fatalf("upstream queries = %d, want 2", got)
			}
		})
	}
}

func TestLookupIPv6NoAddressIsBusinessError(t *testing.T) {
	address := availableDNSAddress(t)
	packetConnection, err := net.ListenPacket("udp4", address.String())
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packetConnection, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		_ = writer.WriteMsg(reply)
	})}
	go server.ActivateAndServe()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownContext(ctx)
	})
	client, err := newClient("lo0", []netip.AddrPort{address}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LookupIPv6(t.Context(), "missing-v6.example")
	if err == nil || !IsBusinessError(err) {
		t.Fatalf("LookupIPv6 error = %v, want business error", err)
	}
}

func TestLookupIPv4TreatsNXDOMAINAsBusinessError(t *testing.T) {
	address := availableDNSAddress(t)
	packetConnection, err := net.ListenPacket("udp4", address.String())
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packetConnection, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetRcode(request, dns.RcodeNameError)
		_ = writer.WriteMsg(reply)
	})}
	go server.ActivateAndServe()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownContext(ctx)
	})
	client, err := newClient("lo0", []netip.AddrPort{address}, time.Second, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LookupIPv4(t.Context(), "missing.example")
	if err == nil || !IsBusinessError(err) {
		t.Fatalf("LookupIPv4 error = %v, want business error", err)
	}
}

func startUDPDNSServer(t *testing.T, handler dns.Handler) netip.AddrPort {
	t.Helper()
	packetConnection, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := packetConnection.LocalAddr().(*net.UDPAddr).AddrPort()
	server := &dns.Server{PacketConn: packetConnection, Handler: handler}
	go server.ActivateAndServe()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownContext(ctx)
	})
	return address
}

func availableDNSAddress(t *testing.T) netip.AddrPort {
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

func availableIPv6DNSAddress(t *testing.T) netip.AddrPort {
	t.Helper()
	listener, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.ParseIP("::1")})
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return netip.AddrPortFrom(netip.IPv6Loopback(), uint16(port))
}
