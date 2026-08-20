// Package resolver performs real DNS queries through explicit upstreams and
// never consults the system resolver that Fake DNS may redirect to loopback.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/miekg/dns"
)

type Client struct {
	servers       []netip.AddrPort
	interfaceName string
	timeout       time.Duration
	semaphore     chan struct{}
	next          atomic.Uint64
	cacheMutex    sync.Mutex
	cache         map[cacheKey]cacheEntry
}

func NewClient(interfaceName string, servers []netip.AddrPort, timeout time.Duration, maxConcurrent int) (*Client, error) {
	return newClient(interfaceName, servers, timeout, maxConcurrent, false)
}

func newClient(interfaceName string, servers []netip.AddrPort, timeout time.Duration, maxConcurrent int, allowLoopback bool) (*Client, error) {
	if interfaceName == "" {
		return nil, errors.New("resolver interface is required")
	}
	if len(servers) == 0 {
		return nil, errors.New("resolver requires explicit upstream servers")
	}
	if timeout <= 0 {
		return nil, errors.New("resolver timeout must be positive")
	}
	if maxConcurrent <= 0 || maxConcurrent > 65536 {
		return nil, fmt.Errorf("resolver max concurrency must be between 1 and 65536, got %d", maxConcurrent)
	}
	serverAddresses := make([]netip.AddrPort, 0, len(servers))
	for _, server := range servers {
		if !server.IsValid() || server.Port() == 0 || server.Addr().IsUnspecified() || server.Addr().Is4In6() || (!allowLoopback && server.Addr().IsLoopback()) {
			return nil, fmt.Errorf("resolver upstream must be non-loopback IPv4 or IPv6 with a port, got %s", server)
		}
		serverAddresses = append(serverAddresses, server)
	}
	return &Client{
		servers:       serverAddresses,
		interfaceName: interfaceName,
		timeout:       timeout,
		semaphore:     make(chan struct{}, maxConcurrent),
		cache:         make(map[cacheKey]cacheEntry),
	}, nil
}

func (client *Client) Exchange(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	select {
	case client.semaphore <- struct{}{}:
		defer func() { <-client.semaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("resolver concurrency limit reached")
	}
	start := int(client.next.Add(1)-1) % len(client.servers)
	var failures []error
	var lastRetryableReply *dns.Msg
	for offset := range len(client.servers) {
		server := client.servers[(start+offset)%len(client.servers)]
		udpNetwork, tcpNetwork := "udp4", "tcp4"
		if server.Addr().Is6() {
			udpNetwork, tcpNetwork = "udp6", "tcp6"
		}
		reply, err := client.exchangeOne(ctx, request, server.String(), udpNetwork)
		if err != nil {
			failures = append(failures, fmt.Errorf("UDP %s: %w", server, err))
			continue
		}
		if reply.Truncated {
			reply, err = client.exchangeOne(ctx, request, server.String(), tcpNetwork)
			if err != nil {
				failures = append(failures, fmt.Errorf("TCP fallback %s: %w", server, err))
				continue
			}
		}
		if isRetryableRCode(reply.Rcode) {
			lastRetryableReply = reply
			failures = append(failures, fmt.Errorf("DNS %s returned %s", server, dns.RcodeToString[reply.Rcode]))
			continue
		}
		return reply, nil
	}
	if lastRetryableReply != nil {
		return lastRetryableReply, nil
	}
	return nil, fmt.Errorf("all explicit DNS upstreams failed: %w", errors.Join(failures...))
}

func isRetryableRCode(rcode int) bool {
	switch rcode {
	case dns.RcodeServerFailure, dns.RcodeRefused, dns.RcodeNotImplemented:
		return true
	default:
		return false
	}
}

func (client *Client) exchangeOne(ctx context.Context, request *dns.Msg, server, network string) (*dns.Msg, error) {
	dialer := &net.Dialer{Timeout: client.timeout, Control: outbound.InterfaceControl(client.interfaceName)}
	dnsClient := &dns.Client{Net: network, Timeout: client.timeout, Dialer: dialer}
	queryCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	reply, _, err := dnsClient.ExchangeContext(queryCtx, request.Copy(), server)
	if err != nil {
		return nil, err
	}
	return reply, nil
}
