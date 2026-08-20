//go:build darwin

package app

import (
	"context"
	"net"
	"sync"
	"sync/atomic"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/fakeip"
	"github.com/hailinpan/tun-proxy/internal/netstack"
	"github.com/hailinpan/tun-proxy/internal/rules"
	"github.com/hailinpan/tun-proxy/internal/session"
)

type dataPlaneGeneration struct {
	tcp  *session.TCP
	udp  *session.UDP
	refs atomic.Int64
}

type dataPlane struct {
	pools      session.MappingPools
	udpLimiter *session.UDPLimiter
	current    atomic.Pointer[dataPlaneGeneration]
	mutex      sync.Mutex
	retired    []*dataPlaneGeneration
	tcpBase    session.Stats
	udpBase    session.UDPStats
}

func newDataPlane(ipv4Pool, ipv6Pool *fakeip.Pool, runtime *config.Config) (*dataPlane, error) {
	pools := session.MappingPools{
		IPv4: ipv4Pool, IPv6: ipv6Pool, IPv4Prefix: runtime.FakeIP.Prefix,
		AllowLiteral: runtime.Capture.DefaultRoute,
	}
	if runtime.FakeIPv6 != nil {
		pools.IPv6Prefix = runtime.FakeIPv6.Prefix
	}
	udpLimiter, err := session.NewUDPLimiter(runtime.Sessions.MaxUDPSessions, runtime.Sessions.MaxUDPSessionsPerSource)
	if err != nil {
		return nil, err
	}
	generation, err := buildDataPlaneGeneration(pools, runtime, udpLimiter)
	if err != nil {
		return nil, err
	}
	plane := &dataPlane{pools: pools, udpLimiter: udpLimiter}
	plane.current.Store(generation)
	return plane, nil
}

func buildDataPlaneGeneration(pools session.MappingPools, runtime *config.Config, udpLimiter *session.UDPLimiter) (*dataPlaneGeneration, error) {
	engine, err := rules.New(runtime.Rules)
	if err != nil {
		return nil, err
	}
	routes, err := session.BuildRoutes(runtime.Outbounds)
	if err != nil {
		return nil, err
	}
	tcp, err := session.NewTCP(pools, engine, routes, runtimeRelayGrace)
	if err != nil {
		return nil, err
	}
	udp, err := session.NewUDPWithLimiter(pools, engine, routes, session.UDPConfig{
		IdleTimeout: runtime.Sessions.UDPIdleTimeout, MaxSessions: runtime.Sessions.MaxUDPSessions,
		MaxPerSource: runtime.Sessions.MaxUDPSessionsPerSource,
	}, udpLimiter)
	if err != nil {
		return nil, err
	}
	return &dataPlaneGeneration{tcp: tcp, udp: udp}, nil
}

func (plane *dataPlane) prepare(runtime *config.Config) (*dataPlaneGeneration, error) {
	return buildDataPlaneGeneration(plane.pools, runtime, plane.udpLimiter)
}

func (plane *dataPlane) commit(next *dataPlaneGeneration) {
	plane.mutex.Lock()
	defer plane.mutex.Unlock()
	previous := plane.current.Swap(next)
	if previous != nil {
		plane.retired = append(plane.retired, previous)
	}
	plane.pruneLocked()
}

func (plane *dataPlane) acquire() *dataPlaneGeneration {
	for {
		generation := plane.current.Load()
		generation.refs.Add(1)
		if plane.current.Load() == generation {
			return generation
		}
		generation.refs.Add(-1)
	}
}

func (plane *dataPlane) handleTCP(ctx context.Context, flow netstack.TCPFlow, client net.Conn) error {
	generation := plane.acquire()
	defer generation.refs.Add(-1)
	return generation.tcp.Handle(ctx, flow, client)
}

func (plane *dataPlane) handleUDP(ctx context.Context, flow netstack.UDPFlow, client net.Conn) error {
	generation := plane.acquire()
	defer generation.refs.Add(-1)
	return generation.udp.Handle(ctx, flow, client)
}

func (plane *dataPlane) stats() (session.Stats, session.UDPStats) {
	plane.mutex.Lock()
	defer plane.mutex.Unlock()
	plane.pruneLocked()
	tcp := plane.tcpBase
	udp := plane.udpBase
	addGenerationStats(&tcp, &udp, plane.current.Load())
	for _, generation := range plane.retired {
		addGenerationStats(&tcp, &udp, generation)
	}
	return tcp, udp
}

func (plane *dataPlane) pruneLocked() {
	retained := plane.retired[:0]
	for _, generation := range plane.retired {
		if generation.refs.Load() != 0 {
			retained = append(retained, generation)
			continue
		}
		tcp, udp := generation.tcp.Stats(), generation.udp.Stats()
		addTCPStats(&plane.tcpBase, tcp)
		addUDPStats(&plane.udpBase, udp)
	}
	plane.retired = retained
}

func addGenerationStats(tcp *session.Stats, udp *session.UDPStats, generation *dataPlaneGeneration) {
	if generation == nil {
		return
	}
	addTCPStats(tcp, generation.tcp.Stats())
	addUDPStats(udp, generation.udp.Stats())
}

func addTCPStats(total *session.Stats, value session.Stats) {
	total.TotalFlows += value.TotalFlows
	total.ActiveFlows += value.ActiveFlows
	total.CompletedFlows += value.CompletedFlows
	total.FailedFlows += value.FailedFlows
	total.FallbackAttempts += value.FallbackAttempts
	total.RejectedFlows += value.RejectedFlows
}

func addUDPStats(total *session.UDPStats, value session.UDPStats) {
	total.TotalSessions += value.TotalSessions
	total.ActiveSessions += value.ActiveSessions
	total.ExpiredSessions += value.ExpiredSessions
	total.FailedSessions += value.FailedSessions
	total.RejectedSessions += value.RejectedSessions
	total.FallbackAttempts += value.FallbackAttempts
	total.DatagramsToNetwork += value.DatagramsToNetwork
	total.DatagramsToClient += value.DatagramsToClient
}
