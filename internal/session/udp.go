package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/hailinpan/tun-proxy/internal/resolver"
	"github.com/hailinpan/tun-proxy/internal/rules"
)

const maxUDPPayload = 65_507

var (
	ErrUDPIdleTimeout = errors.New("UDP session idle timeout")
	ErrUDPCapacity    = errors.New("UDP session capacity reached")
	udpBuffers        = sync.Pool{New: func() any {
		buffer := make([]byte, maxUDPPayload)
		return &buffer
	}}
)

type UDPConfig struct {
	IdleTimeout  time.Duration
	MaxSessions  int
	MaxPerSource int
}

type UDPKey struct {
	SourceIP        netip.Addr
	SourcePort      uint16
	DestinationIP   netip.Addr
	DestinationPort uint16
}

type UDPStats struct {
	TotalSessions      uint64
	ActiveSessions     uint64
	ExpiredSessions    uint64
	FailedSessions     uint64
	RejectedSessions   uint64
	FallbackAttempts   uint64
	DatagramsToNetwork uint64
	DatagramsToClient  uint64
}

type udpCounters struct {
	totalSessions      atomic.Uint64
	activeSessions     atomic.Uint64
	expiredSessions    atomic.Uint64
	failedSessions     atomic.Uint64
	rejectedSessions   atomic.Uint64
	fallbackAttempts   atomic.Uint64
	datagramsToNetwork atomic.Uint64
	datagramsToClient  atomic.Uint64
}

// UDPLimiter owns the process-wide UDP session registry. Multiple policy
// generations can share one limiter without multiplying their configured
// global or per-source capacity.
type UDPLimiter struct {
	mutex        sync.Mutex
	maxSessions  int
	maxPerSource int
	sessions     map[UDPKey]struct{}
	perSource    map[netip.Addr]int
}

type UDP struct {
	pool    MappingPool
	matcher Matcher
	routes  map[string]Route
	config  UDPConfig
	limiter *UDPLimiter

	metrics udpCounters
}

func NewUDP(pool MappingPool, matcher Matcher, routes map[string]Route, config UDPConfig) (*UDP, error) {
	limiter, err := NewUDPLimiter(config.MaxSessions, config.MaxPerSource)
	if err != nil {
		return nil, err
	}
	return NewUDPWithLimiter(pool, matcher, routes, config, limiter)
}

func NewUDPLimiter(maxSessions, maxPerSource int) (*UDPLimiter, error) {
	if maxSessions <= 0 || maxPerSource <= 0 || maxPerSource > maxSessions {
		return nil, errors.New("UDP session limits are invalid")
	}
	return &UDPLimiter{
		maxSessions: maxSessions, maxPerSource: maxPerSource,
		sessions: make(map[UDPKey]struct{}, maxSessions), perSource: make(map[netip.Addr]int),
	}, nil
}

// NewUDPWithLimiter builds a policy generation backed by a shared limiter.
// Capacity settings must remain identical across every generation sharing it.
func NewUDPWithLimiter(pool MappingPool, matcher Matcher, routes map[string]Route, config UDPConfig, limiter *UDPLimiter) (*UDP, error) {
	if pool == nil || matcher == nil {
		return nil, errors.New("UDP session requires a Fake IP pool and rules matcher")
	}
	if config.IdleTimeout <= 0 || config.MaxSessions <= 0 || config.MaxPerSource <= 0 || config.MaxPerSource > config.MaxSessions {
		return nil, errors.New("UDP session limits and idle timeout are invalid")
	}
	if limiter == nil {
		return nil, errors.New("UDP session limiter is required")
	}
	if limiter.maxSessions != config.MaxSessions || limiter.maxPerSource != config.MaxPerSource {
		return nil, errors.New("UDP session limits do not match the shared limiter")
	}
	copyRoutes := make(map[string]Route, len(routes))
	for name, route := range routes {
		if name == "" || route.Name != name {
			return nil, fmt.Errorf("outbound route key %q does not match name %q", name, route.Name)
		}
		if route.Reject {
			if route.Resolver != nil || route.Dialer != nil || route.PacketDialer != nil || route.Fallback != "" {
				return nil, fmt.Errorf("reject route %q cannot resolve, dial, or fallback", name)
			}
		} else if route.Resolver == nil || route.PacketDialer == nil {
			return nil, fmt.Errorf("direct UDP route %q requires resolver and packet dialer", name)
		}
		copyRoutes[name] = route
	}
	if err := validateFallbacks(copyRoutes); err != nil {
		return nil, err
	}
	return &UDP{
		pool: pool, matcher: matcher, routes: copyRoutes, config: config, limiter: limiter,
	}, nil
}

// Handle implements netstack.UDPHandler. The first datagram is already queued
// in client when this method begins.
func (udpSession *UDP) Handle(ctx context.Context, flow netstack.UDPFlow, client net.Conn) error {
	key := UDPKey{
		SourceIP: flow.SourceIP, SourcePort: flow.SourcePort,
		DestinationIP: flow.DestinationIP, DestinationPort: flow.DestinationPort,
	}
	if err := udpSession.register(key); err != nil {
		udpSession.metrics.rejectedSessions.Add(1)
		return err
	}
	udpSession.metrics.totalSessions.Add(1)
	udpSession.metrics.activeSessions.Add(1)
	defer func() {
		udpSession.unregister(key)
		udpSession.metrics.activeSessions.Add(^uint64(0))
	}()

	domain, release, err := udpSession.pool.Acquire(flow.DestinationIP)
	if err != nil {
		udpSession.metrics.failedSessions.Add(1)
		return fmt.Errorf("acquire Fake IP %s for UDP session: %w", flow.DestinationIP, err)
	}
	defer release()
	metadata := rules.FlowMetadata{
		Domain: domain, DestinationIP: flow.DestinationIP, SourceIP: flow.SourceIP,
		SourcePort: flow.SourcePort, DestinationPort: flow.DestinationPort, Protocol: "udp",
	}
	var literal netip.Addr
	if domain == "" {
		literal = flow.DestinationIP
	} else {
		metadata.FakeIP = flow.DestinationIP
	}
	decision, prepared, err := decideFlow(ctx, udpSession.matcher, udpSession.routes, metadata, domain, literal, flow.DestinationIP.Is6(), func() {
		udpSession.metrics.fallbackAttempts.Add(1)
	})
	if err != nil {
		udpSession.metrics.failedSessions.Add(1)
		if errors.Is(err, outbound.ErrRejected) {
			udpSession.metrics.rejectedSessions.Add(1)
		}
		return fmt.Errorf("decide UDP session for %s: %w", domain, err)
	}

	buffer := udpBuffers.Get().(*[]byte)
	defer udpBuffers.Put(buffer)
	if err := client.SetReadDeadline(time.Now().Add(udpSession.config.IdleTimeout)); err != nil {
		return err
	}
	n, err := client.Read(*buffer)
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		if isTimeout(err) {
			udpSession.metrics.expiredSessions.Add(1)
			return nil
		}
		udpSession.metrics.failedSessions.Add(1)
		return fmt.Errorf("read first UDP datagram: %w", err)
	}
	first := (*buffer)[:n]
	upstream, selected, err := udpSession.connectAndSendPrepared(ctx, decision.Outbound, domain, literal, flow.DestinationPort, flow.DestinationIP.Is6(), first, prepared)
	if err != nil {
		udpSession.metrics.failedSessions.Add(1)
		if errors.Is(err, outbound.ErrRejected) {
			udpSession.metrics.rejectedSessions.Add(1)
		}
		return fmt.Errorf("rule %d outbound %q for UDP %s: %w", decision.RuleID, decision.Outbound, domain, err)
	}
	udpSession.metrics.datagramsToNetwork.Add(1)
	defer upstream.Close() //nolint:errcheck // Best-effort cleanup.

	err = relayUDP(ctx, client, upstream, udpSession.config.IdleTimeout, &udpSession.metrics)
	if errors.Is(err, ErrUDPIdleTimeout) {
		udpSession.metrics.expiredSessions.Add(1)
		return nil
	}
	if err != nil {
		udpSession.metrics.failedSessions.Add(1)
		return fmt.Errorf("relay UDP %s through outbound %q: %w", domain, selected, err)
	}
	return nil
}

func (udpSession *UDP) connectAndSend(ctx context.Context, outboundName, domain string, literal netip.Addr, port uint16, ipv6 bool, first []byte) (net.Conn, string, error) {
	return udpSession.connectAndSendPrepared(ctx, outboundName, domain, literal, port, ipv6, first, nil)
}

func (udpSession *UDP) connectAndSendPrepared(ctx context.Context, outboundName, domain string, literal netip.Addr, port uint16, ipv6 bool, first []byte, prepared *routeResolution) (net.Conn, string, error) {
	current := outboundName
	if prepared != nil {
		current = prepared.Outbound
	}
	var failures []error
	for current != "" {
		route, ok := udpSession.routes[current]
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
			failures = append(failures, fmt.Errorf("outbound %q resolve %s: %w", current, domain, err))
			if resolver.IsBusinessError(err) || !outbound.IsRecoverable(err) || route.Fallback == "" {
				return nil, "", errors.Join(failures...)
			}
			udpSession.metrics.fallbackAttempts.Add(1)
			current = route.Fallback
			continue
		}
		allRecoverable := true
		for _, address := range addresses {
			destination := netip.AddrPortFrom(address, port)
			conn, err := route.PacketDialer.DialPacket(ctx, destination)
			if err == nil {
				var written int
				written, err = conn.Write(first)
				if err == nil && written != len(first) {
					err = io.ErrShortWrite
				}
				if err == nil {
					return conn, current, nil
				}
				_ = conn.Close()
			}
			failures = append(failures, fmt.Errorf("outbound %q UDP %s: %w", current, destination, err))
			if !outbound.IsRecoverable(err) {
				allRecoverable = false
			}
		}
		if !allRecoverable || route.Fallback == "" {
			return nil, "", errors.Join(failures...)
		}
		udpSession.metrics.fallbackAttempts.Add(1)
		current = route.Fallback
	}
	return nil, "", errors.Join(failures...)
}

func (udpSession *UDP) register(key UDPKey) error {
	return udpSession.limiter.register(key)
}

func (limiter *UDPLimiter) register(key UDPKey) error {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	if _, exists := limiter.sessions[key]; exists {
		return fmt.Errorf("UDP session %v already exists", key)
	}
	if len(limiter.sessions) >= limiter.maxSessions {
		return ErrUDPCapacity
	}
	if limiter.perSource[key.SourceIP] >= limiter.maxPerSource {
		return fmt.Errorf("%w for source %s", ErrUDPCapacity, key.SourceIP)
	}
	limiter.sessions[key] = struct{}{}
	limiter.perSource[key.SourceIP]++
	return nil
}

func (udpSession *UDP) unregister(key UDPKey) {
	udpSession.limiter.unregister(key)
}

func (limiter *UDPLimiter) unregister(key UDPKey) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	if _, exists := limiter.sessions[key]; !exists {
		return
	}
	delete(limiter.sessions, key)
	limiter.perSource[key.SourceIP]--
	if limiter.perSource[key.SourceIP] == 0 {
		delete(limiter.perSource, key.SourceIP)
	}
}

func (udpSession *UDP) Stats() UDPStats {
	return UDPStats{
		TotalSessions: udpSession.metrics.totalSessions.Load(), ActiveSessions: udpSession.metrics.activeSessions.Load(),
		ExpiredSessions: udpSession.metrics.expiredSessions.Load(), FailedSessions: udpSession.metrics.failedSessions.Load(),
		RejectedSessions: udpSession.metrics.rejectedSessions.Load(), FallbackAttempts: udpSession.metrics.fallbackAttempts.Load(),
		DatagramsToNetwork: udpSession.metrics.datagramsToNetwork.Load(), DatagramsToClient: udpSession.metrics.datagramsToClient.Load(),
	}
}

type udpCopyResult struct{ err error }

func relayUDP(ctx context.Context, client, upstream net.Conn, idleTimeout time.Duration, metrics *udpCounters) error {
	results := make(chan udpCopyResult, 2)
	activity := make(chan struct{}, 1)
	copyDatagrams := func(dst, src net.Conn, counter *atomic.Uint64) {
		buffer := udpBuffers.Get().(*[]byte)
		defer udpBuffers.Put(buffer)
		for {
			n, err := src.Read(*buffer)
			if err != nil {
				results <- udpCopyResult{err: err}
				return
			}
			written, err := dst.Write((*buffer)[:n])
			if err == nil && written != n {
				err = io.ErrShortWrite
			}
			if err != nil {
				results <- udpCopyResult{err: err}
				return
			}
			counter.Add(1)
			select {
			case activity <- struct{}{}:
			default:
			}
		}
	}
	go copyDatagrams(upstream, client, &metrics.datagramsToNetwork)
	go copyDatagrams(client, upstream, &metrics.datagramsToClient)

	closeBoth := sync.OnceFunc(func() {
		_ = client.Close()
		_ = upstream.Close()
	})
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	var result error
	remainingResults := 2
selectLoop:
	for {
		select {
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleTimeout)
		case copyResult := <-results:
			result = copyResult.err
			remainingResults--
			break selectLoop
		case <-timer.C:
			result = ErrUDPIdleTimeout
			break selectLoop
		case <-ctx.Done():
			result = ctx.Err()
			break selectLoop
		}
	}
	closeBoth()
	for range remainingResults {
		select {
		case <-results:
		case <-time.After(time.Second):
			return errors.Join(result, errors.New("UDP relay goroutine did not stop"))
		}
	}
	if errors.Is(result, net.ErrClosed) && ctx.Err() != nil {
		return ctx.Err()
	}
	return result
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
