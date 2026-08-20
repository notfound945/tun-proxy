package app

import (
	"net/netip"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestLimitsFromConfigIncludesPreparedFakeIPv6(t *testing.T) {
	runtime := &config.Config{
		FakeIP:   config.FakeIP{MaxMappings: 1024},
		FakeIPv6: &config.FakeIPv6{Prefix: netip.MustParsePrefix("fd00:7::/96"), MaxMappings: 2048},
		DNS:      config.DNS{MaxConcurrent: 64},
		Sessions: config.Sessions{MaxTCPFlows: 10, MaxUDPSessions: 20, MaxUDPSessionsPerSource: 5},
		TUN:      config.TUN{PacketQueue: 128, BufferPool: 16},
	}
	limits := limitsFromConfig(runtime)
	if limits.FakeIPMappings != 1024 || limits.FakeIPv6Mappings != 2048 {
		t.Fatalf("limits = %+v", limits)
	}
}

func TestRunMonitorReportsIPv6Fallback(t *testing.T) {
	runtime := &config.Config{FakeIPv6: &config.FakeIPv6{}, FakeIP: config.FakeIP{MaxMappings: 1}}
	monitor := newRunMonitor(time.Now(), "sha256:test", runtime, false, "no IPv6 default route")
	snapshot := monitor.snapshot()
	if !snapshot.IPv6.Configured || snapshot.IPv6.Enabled || snapshot.IPv6.FallbackReason != "no IPv6 default route" {
		t.Fatalf("IPv6 status = %+v", snapshot.IPv6)
	}
}
