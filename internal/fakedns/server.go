// Package fakedns serves local UDP/TCP DNS, selectively assigns family-matched
// Fake IP addresses, and forwards all other queries to an explicit resolver.
package fakedns

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hailinpan/tun-proxy/internal/domainname"
	"github.com/hailinpan/tun-proxy/internal/fakeip"
	"github.com/miekg/dns"
)

type Upstream interface {
	Exchange(ctx context.Context, request *dns.Msg) (*dns.Msg, error)
}

type ExchangeFunc func(context.Context, *dns.Msg) (*dns.Msg, error)

func (function ExchangeFunc) Exchange(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	return function(ctx, request)
}

type Config struct {
	Listen        netip.AddrPort
	UDP           bool
	TCP           bool
	TTL           time.Duration
	QueryTimeout  time.Duration
	MaxConcurrent int
	// ShouldFake selects normalized A/AAAA query names for Fake IP answers.
	// A nil callback keeps the legacy behavior of assigning every domain.
	ShouldFake func(domain string) bool
}

type Stats struct {
	Queries         uint64
	FakeAnswers     uint64
	FakeIPv4Answers uint64
	FakeIPv6Answers uint64
	NODATAAnswers   uint64
	Forwarded       uint64
	Failures        uint64
	CapacityRejects uint64
}

type Server struct {
	config          Config
	ipv4Pool        *fakeip.Pool
	ipv6Pool        *fakeip.Pool
	runtime         atomic.Pointer[runtimeConfig]
	semaphore       chan struct{}
	queries         atomic.Uint64
	fakeAnswers     atomic.Uint64
	fakeIPv4Answers atomic.Uint64
	fakeIPv6Answers atomic.Uint64
	nodataAnswers   atomic.Uint64
	forwarded       atomic.Uint64
	failures        atomic.Uint64
	capacityRejects atomic.Uint64
}

type runtimeConfig struct {
	ttl          time.Duration
	queryTimeout time.Duration
	exclude      []domainname.Pattern
	shouldFake   func(domain string) bool
	upstream     Upstream
}

func New(config Config, pool *fakeip.Pool, exclude []domainname.Pattern, upstream Upstream) (*Server, error) {
	return NewDualStack(config, pool, nil, exclude, upstream)
}

// NewDualStack enables Fake AAAA answers only when an IPv6 pool is supplied.
// A nil IPv6 pool deliberately preserves the pre-8.3 AAAA NODATA behavior.
func NewDualStack(config Config, ipv4Pool, ipv6Pool *fakeip.Pool, exclude []domainname.Pattern, upstream Upstream) (*Server, error) {
	if !config.Listen.IsValid() || !config.Listen.Addr().Is4() || !config.Listen.Addr().IsLoopback() || config.Listen.Port() == 0 {
		return nil, fmt.Errorf("Fake DNS listen address must be non-zero IPv4 loopback, got %s", config.Listen)
	}
	if !config.UDP && !config.TCP {
		return nil, errors.New("Fake DNS requires UDP or TCP")
	}
	if config.TTL <= 0 {
		return nil, errors.New("Fake DNS TTL must be positive")
	}
	if config.QueryTimeout == 0 {
		config.QueryTimeout = 5 * time.Second
	}
	if config.QueryTimeout < 0 {
		return nil, errors.New("Fake DNS query timeout must be positive")
	}
	if config.MaxConcurrent <= 0 || config.MaxConcurrent > 65536 {
		return nil, fmt.Errorf("Fake DNS max concurrency must be between 1 and 65536, got %d", config.MaxConcurrent)
	}
	if ipv4Pool == nil {
		return nil, errors.New("Fake IP pool is required")
	}
	if !ipv4Pool.Prefix().Addr().Is4() {
		return nil, errors.New("Fake IPv4 pool must use an IPv4 prefix")
	}
	if ipv6Pool != nil && (!ipv6Pool.Prefix().Addr().Is6() || ipv6Pool.Prefix().Addr().Is4In6()) {
		return nil, errors.New("Fake IPv6 pool must use an IPv6 prefix")
	}
	if upstream == nil {
		return nil, errors.New("explicit upstream resolver is required")
	}
	server := &Server{
		config:    config,
		ipv4Pool:  ipv4Pool,
		ipv6Pool:  ipv6Pool,
		semaphore: make(chan struct{}, config.MaxConcurrent),
	}
	server.runtime.Store(&runtimeConfig{
		ttl: config.TTL, queryTimeout: config.QueryTimeout,
		exclude: append([]domainname.Pattern(nil), exclude...), shouldFake: config.ShouldFake, upstream: upstream,
	})
	return server, nil
}

func (server *Server) ServeDNS(writer dns.ResponseWriter, request *dns.Msg) {
	server.queries.Add(1)
	select {
	case server.semaphore <- struct{}{}:
		defer func() { <-server.semaphore }()
	default:
		server.capacityRejects.Add(1)
		server.writeFailure(writer, request, dns.RcodeServerFailure)
		return
	}
	if len(request.Question) != 1 {
		server.writeFailure(writer, request, dns.RcodeFormatError)
		return
	}
	question := request.Question[0]
	runtime := server.runtime.Load()
	domain, err := domainname.Normalize(question.Name)
	if err != nil {
		server.writeFailure(writer, request, dns.RcodeNameError)
		return
	}
	if excluded(runtime.exclude, domain) ||
		(question.Qtype != dns.TypeA && question.Qtype != dns.TypeAAAA) ||
		(runtime.shouldFake != nil && !runtime.shouldFake(domain)) {
		server.forward(writer, request, runtime)
		return
	}

	reply := new(dns.Msg)
	reply.SetReply(request)
	reply.Authoritative = true
	pool := server.ipv4Pool
	if question.Qtype == dns.TypeAAAA {
		pool = server.ipv6Pool
	}
	if pool == nil {
		server.nodataAnswers.Add(1)
		if err := writer.WriteMsg(reply); err != nil {
			server.failures.Add(1)
		}
		return
	}
	address, err := pool.GetOrAllocate(domain)
	if err != nil {
		server.writeFailure(writer, request, dns.RcodeServerFailure)
		return
	}
	ttlSeconds := runtime.ttl / time.Second
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	if ttlSeconds > math.MaxUint32 {
		ttlSeconds = math.MaxUint32
	}
	if question.Qtype == dns.TypeAAAA {
		reply.Answer = []dns.RR{&dns.AAAA{
			Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: uint32(ttlSeconds)},
			AAAA: net.IP(address.AsSlice()),
		}}
		server.fakeIPv6Answers.Add(1)
	} else {
		reply.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(ttlSeconds)},
			A:   net.IP(address.AsSlice()),
		}}
		server.fakeIPv4Answers.Add(1)
	}
	server.fakeAnswers.Add(1)
	if err := writer.WriteMsg(reply); err != nil {
		server.failures.Add(1)
	}
}

func (server *Server) forward(writer dns.ResponseWriter, request *dns.Msg, runtime *runtimeConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), runtime.queryTimeout)
	defer cancel()
	reply, err := runtime.upstream.Exchange(ctx, request.Copy())
	if err != nil || reply == nil {
		server.writeFailure(writer, request, dns.RcodeServerFailure)
		return
	}
	reply.Id = request.Id
	server.forwarded.Add(1)
	if err := writer.WriteMsg(reply); err != nil {
		server.failures.Add(1)
	}
}

func (server *Server) writeFailure(writer dns.ResponseWriter, request *dns.Msg, rcode int) {
	server.failures.Add(1)
	reply := new(dns.Msg)
	reply.SetRcode(request, rcode)
	_ = writer.WriteMsg(reply)
}

func excluded(patterns []domainname.Pattern, domain string) bool {
	for _, pattern := range patterns {
		if pattern.Matches(domain) {
			return true
		}
	}
	return false
}

// Reload atomically replaces resolver, exclusions, TTL and timeout while
// preserving the current Fake IP selection policy.
func (server *Server) Reload(ttl, queryTimeout time.Duration, exclude []domainname.Pattern, upstream Upstream) error {
	return server.ReloadPolicy(ttl, queryTimeout, exclude, server.runtime.Load().shouldFake, upstream)
}

// ReloadPolicy atomically replaces all query-time behavior. A query already
// in progress keeps the resolver, exclusions, policy, TTL and timeout snapshot
// it started with. A nil policy preserves the legacy all-domains Fake IP mode.
func (server *Server) ReloadPolicy(
	ttl, queryTimeout time.Duration,
	exclude []domainname.Pattern,
	shouldFake func(domain string) bool,
	upstream Upstream,
) error {
	if ttl <= 0 {
		return errors.New("Fake DNS TTL must be positive")
	}
	if queryTimeout <= 0 {
		return errors.New("Fake DNS query timeout must be positive")
	}
	if upstream == nil {
		return errors.New("explicit upstream resolver is required")
	}
	server.runtime.Store(&runtimeConfig{
		ttl: ttl, queryTimeout: queryTimeout,
		exclude: append([]domainname.Pattern(nil), exclude...), shouldFake: shouldFake, upstream: upstream,
	})
	return nil
}

func (server *Server) Stats() Stats {
	return Stats{
		Queries:         server.queries.Load(),
		FakeAnswers:     server.fakeAnswers.Load(),
		FakeIPv4Answers: server.fakeIPv4Answers.Load(),
		FakeIPv6Answers: server.fakeIPv6Answers.Load(),
		NODATAAnswers:   server.nodataAnswers.Load(),
		Forwarded:       server.forwarded.Load(),
		Failures:        server.failures.Load(),
		CapacityRejects: server.capacityRejects.Load(),
	}
}

type Running struct {
	servers   []*dns.Server
	wait      sync.WaitGroup
	done      chan struct{}
	stopped   chan struct{}
	doneOnce  sync.Once
	closing   atomic.Bool
	errMutex  sync.Mutex
	runErr    error
	closeOnce sync.Once
	closeErr  error
}

// Listeners contains already-bound Fake DNS sockets. A privileged supervisor
// can bind port 53 and pass the descriptors to an unprivileged worker, keeping
// socket creation outside the policy and relay process.
type Listeners struct {
	UDP net.PacketConn
	TCP net.Listener
}

// Start binds every requested listener before launching either server. If one
// bind fails, all earlier listeners are closed and no partial service remains.
func (server *Server) Start() (*Running, error) {
	address := server.config.Listen.String()
	listeners := Listeners{}
	if server.config.UDP {
		connection, err := net.ListenPacket("udp4", address)
		if err != nil {
			return nil, fmt.Errorf("listen Fake DNS UDP %s: %w", address, err)
		}
		listeners.UDP = connection
	}
	if server.config.TCP {
		listener, err := net.Listen("tcp4", address)
		if err != nil {
			if listeners.UDP != nil {
				_ = listeners.UDP.Close()
			}
			return nil, fmt.Errorf("listen Fake DNS TCP %s: %w", address, err)
		}
		listeners.TCP = listener
	}
	running, err := server.StartWithListeners(listeners)
	if err != nil {
		_ = closeListeners(listeners)
		return nil, err
	}
	return running, nil
}

// StartWithListeners serves Fake DNS on sockets bound by the caller. On
// success Running owns both sockets. On validation failure ownership remains
// with the caller so a supervisor can perform deterministic rollback. Once
// activation begins, this method closes both sockets if either server fails to
// start.
func (server *Server) StartWithListeners(listeners Listeners) (*Running, error) {
	if server.config.UDP != (listeners.UDP != nil) {
		return nil, fmt.Errorf("Fake DNS UDP listener presence does not match udp=%t", server.config.UDP)
	}
	if server.config.TCP != (listeners.TCP != nil) {
		return nil, fmt.Errorf("Fake DNS TCP listener presence does not match tcp=%t", server.config.TCP)
	}
	if listeners.UDP != nil {
		if err := validateListenerAddress("UDP", listeners.UDP.LocalAddr(), server.config.Listen); err != nil {
			return nil, err
		}
	}
	if listeners.TCP != nil {
		if err := validateListenerAddress("TCP", listeners.TCP.Addr(), server.config.Listen); err != nil {
			return nil, err
		}
	}

	running := &Running{done: make(chan struct{}), stopped: make(chan struct{})}
	type configuredServer struct {
		name   string
		server *dns.Server
	}
	configured := make([]configuredServer, 0, 2)
	if listeners.UDP != nil {
		configured = append(configured, configuredServer{name: "UDP", server: &dns.Server{PacketConn: listeners.UDP, Handler: server}})
	}
	if listeners.TCP != nil {
		configured = append(configured, configuredServer{name: "TCP", server: &dns.Server{Listener: listeners.TCP, Handler: server}})
	}
	started := make([]chan struct{}, len(configured))
	exited := make([]chan error, len(configured))
	for index, configuredServer := range configured {
		running.servers = append(running.servers, configuredServer.server)
		started[index] = make(chan struct{})
		exited[index] = make(chan error, 1)
		configuredServer.server.NotifyStartedFunc = func() { close(started[index]) }
		running.wait.Add(1)
		go func(index int, name string, dnsServer *dns.Server) {
			defer running.wait.Done()
			err := dnsServer.ActivateAndServe()
			if !running.closing.Load() {
				if err == nil {
					err = errors.New("stopped unexpectedly")
				}
				running.recordError(fmt.Errorf("Fake DNS %s server: %w", name, err))
			}
			exited[index] <- err
			running.doneOnce.Do(func() { close(running.done) })
		}(index, configuredServer.name, configuredServer.server)
	}
	go func() {
		running.wait.Wait()
		close(running.stopped)
	}()
	for index := range configured {
		select {
		case <-started[index]:
			// NotifyStartedFunc runs immediately before the serving loop. Prefer
			// an already-observed exit if both startup signals are ready.
			select {
			case err := <-exited[index]:
				closeErr := closeListeners(listeners)
				<-running.stopped
				return nil, errors.Join(fmt.Errorf("start Fake DNS server: %w", err), closeErr)
			default:
			}
		case err := <-exited[index]:
			closeErr := closeListeners(listeners)
			<-running.stopped
			return nil, errors.Join(fmt.Errorf("start Fake DNS server: %w", err), closeErr)
		}
	}
	select {
	case <-running.done:
		closeErr := closeListeners(listeners)
		<-running.stopped
		return nil, errors.Join(fmt.Errorf("start Fake DNS server: %w", running.Err()), closeErr)
	default:
		return running, nil
	}
}

func validateListenerAddress(protocol string, address net.Addr, expected netip.AddrPort) error {
	actual, err := netip.ParseAddrPort(address.String())
	if err != nil {
		return fmt.Errorf("parse inherited Fake DNS %s listener address %q: %w", protocol, address, err)
	}
	if actual != expected {
		return fmt.Errorf("inherited Fake DNS %s listener is %s, want %s", protocol, actual, expected)
	}
	return nil
}

func closeListeners(listeners Listeners) error {
	var failures []error
	if listeners.UDP != nil {
		failures = append(failures, listeners.UDP.Close())
	}
	if listeners.TCP != nil {
		failures = append(failures, listeners.TCP.Close())
	}
	return errors.Join(failures...)
}

// Done closes when any UDP or TCP serving loop exits. Callers must treat an
// exit before Close as fatal because leaving only one DNS transport alive can
// break host resolution after the system resolver has been redirected here.
func (running *Running) Done() <-chan struct{} { return running.done }

// Err returns the first unexpected serving-loop failure, if any.
func (running *Running) Err() error {
	running.errMutex.Lock()
	defer running.errMutex.Unlock()
	return running.runErr
}

func (running *Running) recordError(err error) {
	running.errMutex.Lock()
	defer running.errMutex.Unlock()
	if running.runErr == nil {
		running.runErr = err
	}
}

func (running *Running) Close(ctx context.Context) error {
	running.closeOnce.Do(func() {
		running.closing.Store(true)
		var failures []error
		for _, server := range running.servers {
			if err := server.ShutdownContext(ctx); err != nil {
				failures = append(failures, err)
			}
		}
		running.closeErr = errors.Join(failures...)
	})
	select {
	case <-running.stopped:
		return running.closeErr
	case <-ctx.Done():
		if errors.Is(running.closeErr, ctx.Err()) {
			return running.closeErr
		}
		return errors.Join(running.closeErr, ctx.Err())
	}
}
