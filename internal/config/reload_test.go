package config

import (
	"net/netip"
	"strings"
	"testing"
)

func TestValidateReloadAllowsMutableConfiguration(t *testing.T) {
	current := loadValidConfig(t)
	next := loadValidConfig(t)
	next.Log.Level = "debug"
	next.FakeIP.DNSTTL *= 2
	next.DNS.DefaultOutbound = "secondary"
	next.Sessions.UDPIdleTimeout *= 2
	primary := next.Outbounds["primary"]
	primary.ConnectTimeout *= 2
	next.Outbounds["primary"] = primary
	next.Rules[0].Outbound = "primary"
	if err := ValidateReload(current, next); err != nil {
		t.Fatalf("ValidateReload() = %v", err)
	}
}

func TestValidateReloadAllowsIPCIDRRuleGeneration(t *testing.T) {
	current := loadValidConfig(t)
	next := loadValidConfig(t)
	next.Rules = []Rule{
		{ID: 1, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "secondary"},
		{ID: 2, Outbound: "primary"},
	}
	if err := ValidateReload(current, next); err != nil {
		t.Fatalf("ValidateReload() = %v", err)
	}
}

func TestValidateReloadRejectsImmutableConfiguration(t *testing.T) {
	current := loadValidConfig(t)
	next := loadValidConfig(t)
	next.TUN.MTU++
	next.TUN.IPv6Address = netip.MustParseAddr("fd00:ffff::2")
	next.FakeIP.PersistenceFile += ".next"
	next.Sessions.MaxUDPSessions++
	err := ValidateReload(current, next)
	if err == nil || !strings.Contains(err.Error(), "tun.mtu") || !strings.Contains(err.Error(), "tun.ipv6_address") || !strings.Contains(err.Error(), "fake_ip.persistence_file") || !strings.Contains(err.Error(), "sessions.max_udp_sessions") {
		t.Fatalf("ValidateReload() = %v", err)
	}
}

func TestValidateReloadFakeIPv6MustRemainIdentical(t *testing.T) {
	current := loadValidConfig(t)
	next := loadValidConfig(t)
	fakeIPv6 := FakeIPv6{
		Prefix: netip.MustParsePrefix("fd00:7::/96"), MaxMappings: 1024,
		PersistenceFile: "/tmp/tun-proxy/fake-ipv6.yaml",
	}
	current.FakeIPv6 = &fakeIPv6
	nextFakeIPv6 := fakeIPv6
	next.FakeIPv6 = &nextFakeIPv6
	if err := ValidateReload(current, next); err != nil {
		t.Fatalf("identical fake_ipv6 rejected: %v", err)
	}
	next.FakeIPv6.MaxMappings++
	if err := ValidateReload(current, next); err == nil || !strings.Contains(err.Error(), "fake_ipv6.max_mappings") {
		t.Fatalf("changed fake_ipv6 = %v", err)
	}
}

func TestValidateReloadProtectsDefaultRouteCaptureTopology(t *testing.T) {
	current := loadValidConfig(t)
	next := loadValidConfig(t)
	current.Capture.DefaultRoute = true
	next.Capture.DefaultRoute = true
	primary := next.Outbounds["primary"]
	primary.DNS = []netip.AddrPort{netip.MustParseAddrPort("8.8.8.8:53")}
	next.Outbounds["primary"] = primary
	err := ValidateReload(current, next)
	if err == nil || !strings.Contains(err.Error(), "outbounds cannot be reloaded") {
		t.Fatalf("ValidateReload() = %v", err)
	}
}

func loadValidConfig(t *testing.T) *Config {
	t.Helper()
	configuration, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}
