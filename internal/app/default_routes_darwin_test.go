//go:build darwin

package app

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/system"
)

func TestPlanDefaultRouteCaptureBuildsGatewayDNSAndSplitRoutes(t *testing.T) {
	runtime := captureTestConfig()
	lookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		gateway := "192.0.2.1"
		if address.Is6() {
			gateway = "2001:db8::1"
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: gateway}, nil
	}
	lookupDefault := func(_ context.Context, ipv6 bool, interfaceName string) (system.RouteLookup, error) {
		gateway := "192.0.2.1"
		if ipv6 {
			gateway = "2001:db8::1"
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: gateway}, nil
	}
	plan, err := planDefaultRouteCapture(t.Context(), runtime, true, lookup, lookupDefault)
	if err != nil {
		t.Fatal(err)
	}
	wantBypasses := []system.RouteState{
		{Prefix: "192.0.2.1/32", Interface: "en0"},
		{Prefix: "2001:db8::1/128", Interface: "en0"},
		{Prefix: "1.1.1.1/32", Interface: "en0", Gateway: "192.0.2.1"},
		{Prefix: "2001:4860:4860::8888/128", Interface: "en0", Gateway: "2001:db8::1"},
	}
	if len(plan.Bypasses) != len(wantBypasses) {
		t.Fatalf("bypasses = %+v", plan.Bypasses)
	}
	for index := range wantBypasses {
		if plan.Bypasses[index] != wantBypasses[index] {
			t.Fatalf("bypass %d = %+v, want %+v", index, plan.Bypasses[index], wantBypasses[index])
		}
	}
	wantScoped := []system.RouteState{
		{Prefix: "0.0.0.0/1", Interface: "en0", Gateway: "192.0.2.1", Scope: "en0"},
		{Prefix: "128.0.0.0/1", Interface: "en0", Gateway: "192.0.2.1", Scope: "en0"},
		{Prefix: "8000::/1", Interface: "en0", Gateway: "2001:db8::1", Scope: "en0"},
		{Prefix: "::/1", Interface: "en0", Gateway: "2001:db8::1", Scope: "en0"},
	}
	if len(plan.Scoped) != len(wantScoped) {
		t.Fatalf("scoped routes = %+v", plan.Scoped)
	}
	for index := range wantScoped {
		if plan.Scoped[index] != wantScoped[index] {
			t.Fatalf("scoped route %d = %+v, want %+v", index, plan.Scoped[index], wantScoped[index])
		}
	}
	routes := plan.routes("utun7")
	if len(routes) != 12 || routes[8].Prefix != "0.0.0.0/1" || routes[9].Prefix != "128.0.0.0/1" || routes[10].Prefix != "::/1" || routes[11].Prefix != "8000::/1" {
		t.Fatalf("routes = %+v", routes)
	}
}

func TestPlanDefaultRouteCaptureKeepsScopedPrefixesPerInterface(t *testing.T) {
	runtime := captureTestConfig()
	runtime.Outbounds["other"] = config.Outbound{
		Name: "other", Type: "direct", Interface: "en7", ConnectTimeout: time.Second,
	}
	lookup := func(_ context.Context, _ netip.Addr, interfaceName string) (system.RouteLookup, error) {
		gateway := "192.0.2.1"
		if interfaceName == "en7" {
			gateway = "198.51.100.1"
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: gateway}, nil
	}
	lookupDefault := func(_ context.Context, _ bool, interfaceName string) (system.RouteLookup, error) {
		return lookup(t.Context(), netip.Addr{}, interfaceName)
	}
	plan, err := planDefaultRouteCapture(t.Context(), runtime, false, lookup, lookupDefault)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Scoped) != 4 {
		t.Fatalf("scoped routes = %+v", plan.Scoped)
	}
	seen := make(map[string]bool)
	for _, route := range plan.Scoped {
		seen[route.Prefix+"|"+route.Scope] = true
	}
	for _, key := range []string{"0.0.0.0/1|en0", "128.0.0.0/1|en0", "0.0.0.0/1|en7", "128.0.0.0/1|en7"} {
		if !seen[key] {
			t.Fatalf("missing scoped route %s in %+v", key, plan.Scoped)
		}
	}
}

func TestPlanDefaultRouteCaptureReprovesInstalledScopedSplitRoutes(t *testing.T) {
	runtime := captureTestConfig()
	outbound := runtime.Outbounds["direct"]
	outbound.DNS = outbound.DNS[:1]
	runtime.Outbounds["direct"] = outbound
	lookupDefault := func(_ context.Context, _ bool, interfaceName string) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}, nil
	}
	initialLookup := func(_ context.Context, _ netip.Addr, interfaceName string) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1", Mask: "default"}, nil
	}
	initial, err := planDefaultRouteCapture(t.Context(), runtime, false, initialLookup, lookupDefault)
	if err != nil {
		t.Fatal(err)
	}
	installedLookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		result := system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1", Mask: "128.0.0.0"}
		if address.String() == "192.0.2.1" {
			result.Destination = address.String()
			result.Mask = ""
			result.Gateway = "link#4"
		} else if address.String() == "1.1.1.1" {
			result.Destination = address.String()
			result.Mask = ""
		}
		return result, nil
	}
	reproved, err := planDefaultRouteCaptureOwned(t.Context(), runtime, false, installedLookup, lookupDefault, initial.Bypasses)
	if err != nil {
		t.Fatal(err)
	}
	if !initial.equal(reproved) {
		t.Fatalf("reproved plan = %+v, want %+v", reproved, initial)
	}
}

func TestPlanDefaultRouteCaptureRejectsAmbiguousBypass(t *testing.T) {
	runtime := captureTestConfig()
	runtime.Outbounds["other"] = config.Outbound{
		Name: "other", Type: "direct", Interface: "en7",
		DNS: []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:53")}, ConnectTimeout: time.Second,
	}
	lookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		gateway := "192.0.2.1"
		if interfaceName == "en7" {
			gateway = "198.51.100.1"
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: gateway}, nil
	}
	lookupDefault := func(_ context.Context, _ bool, interfaceName string) (system.RouteLookup, error) {
		gateway := "192.0.2.1"
		if interfaceName == "en7" {
			gateway = "198.51.100.1"
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: gateway}, nil
	}
	_, err := planDefaultRouteCapture(t.Context(), runtime, false, lookup, lookupDefault)
	if err == nil || !strings.Contains(err.Error(), "route ownership is ambiguous") {
		t.Fatalf("planDefaultRouteCapture() = %v", err)
	}
}

func TestPlanDefaultRouteCaptureSupportsDirectlyConnectedDNS(t *testing.T) {
	runtime := captureTestConfig()
	outbound := runtime.Outbounds["direct"]
	outbound.DNS = []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:53")}
	runtime.Outbounds["direct"] = outbound
	lookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		if address.String() == "10.0.0.1" {
			return system.RouteLookup{Destination: "10.0.0.0/24", Interface: interfaceName, Gateway: "link#4"}, nil
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}, nil
	}
	lookupDefault := func(_ context.Context, _ bool, interfaceName string) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}, nil
	}
	plan, err := planDefaultRouteCapture(t.Context(), runtime, false, lookup, lookupDefault)
	if err != nil {
		t.Fatal(err)
	}
	want := []system.RouteState{
		{Prefix: "192.0.2.1/32", Interface: "en0"},
		{Prefix: "10.0.0.1/32", Interface: "en0"},
	}
	if len(plan.Bypasses) != len(want) || plan.Bypasses[0] != want[0] || plan.Bypasses[1] != want[1] {
		t.Fatalf("bypasses = %+v, want %+v", plan.Bypasses, want)
	}
}

func TestPlanDefaultRouteCaptureReusesSystemGatewayHostRoute(t *testing.T) {
	runtime := captureTestConfig()
	outbound := runtime.Outbounds["direct"]
	outbound.DNS = outbound.DNS[:1]
	runtime.Outbounds["direct"] = outbound
	lookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		if address.String() == "192.0.2.1" {
			return system.RouteLookup{Destination: address.String(), Interface: interfaceName}, nil
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}, nil
	}
	lookupDefault := func(_ context.Context, _ bool, interfaceName string) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}, nil
	}
	plan, err := planDefaultRouteCapture(t.Context(), runtime, false, lookup, lookupDefault)
	if err != nil {
		t.Fatal(err)
	}
	want := system.RouteState{Prefix: "1.1.1.1/32", Interface: "en0", Gateway: "192.0.2.1"}
	if len(plan.Bypasses) != 1 || plan.Bypasses[0] != want {
		t.Fatalf("bypasses = %+v, want only %+v", plan.Bypasses, want)
	}
}

func TestPlanDefaultRouteCapturePlansIPv6GatewayWithoutIPv6DNS(t *testing.T) {
	runtime := captureTestConfig()
	outbound := runtime.Outbounds["direct"]
	outbound.DNS = outbound.DNS[:1]
	runtime.Outbounds["direct"] = outbound
	lookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		gateway := "192.0.2.1"
		if address.Is6() {
			gateway = "fe80::1%en0"
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: gateway}, nil
	}
	lookupDefault := func(_ context.Context, ipv6 bool, interfaceName string) (system.RouteLookup, error) {
		gateway := "192.0.2.1"
		if ipv6 {
			gateway = "fe80::1%en0"
		}
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: gateway}, nil
	}
	plan, err := planDefaultRouteCapture(t.Context(), runtime, true, lookup, lookupDefault)
	if err != nil {
		t.Fatal(err)
	}
	foundIPv6Gateway := false
	for _, route := range plan.Bypasses {
		if route.Prefix == "fe80::1/128" && route.Interface == "en0" {
			foundIPv6Gateway = true
		}
	}
	if !foundIPv6Gateway || len(plan.Prefixes) != 4 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanDefaultRouteCaptureReprovesOwnedHostRoutes(t *testing.T) {
	runtime := captureTestConfig()
	outbound := runtime.Outbounds["direct"]
	outbound.DNS = outbound.DNS[:1]
	runtime.Outbounds["direct"] = outbound
	initialLookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}, nil
	}
	lookupDefault := func(_ context.Context, _ bool, interfaceName string) (system.RouteLookup, error) {
		return system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}, nil
	}
	initial, err := planDefaultRouteCapture(t.Context(), runtime, false, initialLookup, lookupDefault)
	if err != nil {
		t.Fatal(err)
	}
	ownedLookup := func(_ context.Context, address netip.Addr, interfaceName string) (system.RouteLookup, error) {
		result := system.RouteLookup{Destination: "default", Interface: interfaceName, Gateway: "192.0.2.1"}
		if address.String() == "1.1.1.1" {
			result.Destination = address.String()
			result.Gateway = "192.0.2.1"
		} else if address.String() == "192.0.2.1" {
			result.Destination = address.String()
			result.Gateway = "link#4"
		}
		return result, nil
	}
	reproved, err := planDefaultRouteCaptureOwned(t.Context(), runtime, false, ownedLookup, lookupDefault, initial.Bypasses)
	if err != nil {
		t.Fatal(err)
	}
	if !initial.equal(reproved) {
		t.Fatalf("reproved plan = %+v, want %+v", reproved, initial)
	}
}

func captureTestConfig() *config.Config {
	return &config.Config{
		Capture: config.Capture{DefaultRoute: true},
		Outbounds: map[string]config.Outbound{
			"direct": {
				Name: "direct", Type: "direct", Interface: "en0", ConnectTimeout: time.Second,
				DNS: []netip.AddrPort{
					netip.MustParseAddrPort("1.1.1.1:53"),
					netip.MustParseAddrPort("[2001:4860:4860::8888]:53"),
				},
			},
		},
	}
}
