//go:build darwin

package app

import (
	"net/netip"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/fakeip"
)

func TestDataPlaneRetiresInactiveReloadGenerations(t *testing.T) {
	pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &config.Config{
		Sessions: config.Sessions{UDPIdleTimeout: time.Minute, MaxUDPSessions: 8, MaxUDPSessionsPerSource: 4},
		Outbounds: map[string]config.Outbound{
			"direct": {Name: "direct", Type: "direct", Interface: "lo0", DNS: []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:53")}, ConnectTimeout: time.Second},
		},
		Rules: []config.Rule{{ID: 1, Outbound: "direct"}},
	}
	plane, err := newDataPlane(pool, nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		next, err := plane.prepare(runtime)
		if err != nil {
			t.Fatal(err)
		}
		plane.commit(next)
	}
	if len(plane.retired) != 0 {
		t.Fatalf("inactive generations retained = %d", len(plane.retired))
	}
}

func TestDataPlaneReloadKeepsActiveCIDRGenerationUntilFlowRelease(t *testing.T) {
	pool, err := fakeip.New(netip.MustParsePrefix("198.18.0.0/24"), time.Hour, 32, 10)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &config.Config{
		Sessions: config.Sessions{UDPIdleTimeout: time.Minute, MaxUDPSessions: 8, MaxUDPSessionsPerSource: 4},
		Outbounds: map[string]config.Outbound{
			"direct": {Name: "direct", Type: "direct", Interface: "lo0", DNS: []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:53")}, ConnectTimeout: time.Second},
		},
		Rules: []config.Rule{{ID: 1, Outbound: "direct"}},
	}
	plane, err := newDataPlane(pool, nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	active := plane.acquire()
	nextRuntime := *runtime
	nextRuntime.Rules = []config.Rule{
		{ID: 1, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "direct"},
		{ID: 2, Outbound: "direct"},
	}
	next, err := plane.prepare(&nextRuntime)
	if err != nil {
		t.Fatal(err)
	}
	plane.commit(next)
	if plane.current.Load() != next || len(plane.retired) != 1 || plane.retired[0] != active {
		t.Fatalf("current=%p retired=%v active=%p", plane.current.Load(), plane.retired, active)
	}
	current := plane.acquire()
	if current != next {
		t.Fatalf("new flow acquired generation %p, want %p", current, next)
	}
	current.refs.Add(-1)
	active.refs.Add(-1)
	plane.stats()
	if len(plane.retired) != 0 {
		t.Fatalf("released generation retained = %v", plane.retired)
	}
}
