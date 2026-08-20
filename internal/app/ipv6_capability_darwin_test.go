//go:build darwin

package app

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/interfaceinfo"
	"github.com/hailinpan/tun-proxy/internal/system"
)

func TestIPv6DataPathFallsBackWithoutUsableInterfaceAddress(t *testing.T) {
	runtime := ipv6CapabilityConfig()
	get := func(name string) (interfaceinfo.Interface, error) {
		return interfaceinfo.Interface{Name: name, Addresses: []string{"192.0.2.10/24", "fe80::1%" + name + "/64"}}, nil
	}
	enabled, reason := ipv6DataPathAvailable(t.Context(), runtime, get, func(context.Context, bool) (system.RouteLookup, error) {
		t.Fatal("route lookup must not run without a usable interface address")
		return system.RouteLookup{}, nil
	})
	if enabled || !strings.Contains(reason, "no usable") {
		t.Fatalf("enabled=%t reason=%q", enabled, reason)
	}
}

func TestIPv6DataPathFallsBackWithoutDefaultRoute(t *testing.T) {
	runtime := ipv6CapabilityConfig()
	get := func(name string) (interfaceinfo.Interface, error) {
		return interfaceinfo.Interface{Name: name, Addresses: []string{"2001:db8:1::10/64"}}, nil
	}
	enabled, reason := ipv6DataPathAvailable(t.Context(), runtime, get, func(_ context.Context, ipv6 bool) (system.RouteLookup, error) {
		if !ipv6 {
			t.Fatal("IPv6 default route lookup used IPv4 family")
		}
		return system.RouteLookup{}, system.ErrRouteNotFound
	})
	if enabled || !strings.Contains(reason, "no IPv6 default route") {
		t.Fatalf("enabled=%t reason=%q", enabled, reason)
	}
}

func TestIPv6DataPathRequiresConfiguredCapableDefaultInterface(t *testing.T) {
	runtime := ipv6CapabilityConfig()
	get := func(name string) (interfaceinfo.Interface, error) {
		return interfaceinfo.Interface{Name: name, Addresses: []string{"2001:db8:1::10/64"}}, nil
	}
	lookup := func(context.Context, bool) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: "en9"}, nil
	}
	enabled, reason := ipv6DataPathAvailable(t.Context(), runtime, get, lookup)
	if enabled || !strings.Contains(reason, "en9") {
		t.Fatalf("enabled=%t reason=%q", enabled, reason)
	}
}

func TestIPv6DataPathEnablesOnConfiguredDefaultInterface(t *testing.T) {
	runtime := ipv6CapabilityConfig()
	get := func(name string) (interfaceinfo.Interface, error) {
		if name != "en0" {
			return interfaceinfo.Interface{}, errors.New("unexpected interface")
		}
		return interfaceinfo.Interface{Name: name, Addresses: []string{"2001:db8:1::10/64"}}, nil
	}
	lookup := func(context.Context, bool) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: "en0"}, nil
	}
	enabled, reason := ipv6DataPathAvailable(t.Context(), runtime, get, lookup)
	if !enabled || reason != "" {
		t.Fatalf("enabled=%t reason=%q", enabled, reason)
	}
}

func ipv6CapabilityConfig() *config.Config {
	return &config.Config{
		FakeIPv6: &config.FakeIPv6{Prefix: netip.MustParsePrefix("fd00:7::/96")},
		Outbounds: map[string]config.Outbound{
			"direct": {Name: "direct", Type: "direct", Interface: "en0"},
			"reject": {Name: "reject", Type: "reject"},
		},
	}
}
