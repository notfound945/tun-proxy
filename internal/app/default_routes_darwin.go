//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/system"
)

var (
	ipv4CapturePrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/1"),
		netip.MustParsePrefix("128.0.0.0/1"),
	}
	ipv6CapturePrefixes = []netip.Prefix{
		netip.MustParsePrefix("::/1"),
		netip.MustParsePrefix("8000::/1"),
	}
)

type defaultRoutePlan struct {
	Bypasses []system.RouteState
	Scoped   []system.RouteState
	Prefixes []netip.Prefix
}

type scopedRouteLookup func(context.Context, netip.Addr, string) (system.RouteLookup, error)
type scopedDefaultRouteLookup func(context.Context, bool, string) (system.RouteLookup, error)

// planDefaultRouteCapture proves every configured upstream DNS endpoint has a
// physical-interface route before any split-default route is installed. The
// local control plane uses a Unix socket, so it has no IP endpoint to bypass.
func planDefaultRouteCapture(ctx context.Context, runtime *config.Config, ipv6Enabled bool, lookup scopedRouteLookup, lookupDefault scopedDefaultRouteLookup) (defaultRoutePlan, error) {
	return planDefaultRouteCaptureOwned(ctx, runtime, ipv6Enabled, lookup, lookupDefault, nil)
}

func planDefaultRouteCaptureOwned(ctx context.Context, runtime *config.Config, ipv6Enabled bool, lookup scopedRouteLookup, lookupDefault scopedDefaultRouteLookup, owned []system.RouteState) (defaultRoutePlan, error) {
	if runtime == nil || !runtime.Capture.DefaultRoute {
		return defaultRoutePlan{}, nil
	}
	if lookup == nil || lookupDefault == nil {
		return defaultRoutePlan{}, errors.New("default-route capture requires scoped route lookups")
	}

	names := make([]string, 0, len(runtime.Outbounds))
	for name := range runtime.Outbounds {
		names = append(names, name)
	}
	sort.Strings(names)

	owners := make(map[netip.Addr]string)
	gatewayRoutes := make(map[string]system.RouteState)
	dnsRoutes := make(map[string]system.RouteState)
	scopedRoutes := make(map[string]system.RouteState)
	defaultGateways := make(map[string]netip.Addr)
	for _, name := range names {
		outbound := runtime.Outbounds[name]
		if outbound.Type != "direct" {
			continue
		}
		families := []bool{false}
		if ipv6Enabled {
			families = append(families, true)
		}
		for _, ipv6 := range families {
			familyKey := outbound.Interface + "|4"
			probeAddress := netip.MustParseAddr("192.0.2.254")
			if ipv6 {
				familyKey = outbound.Interface + "|6"
				probeAddress = netip.MustParseAddr("2001:db8::fffe")
			}
			if _, exists := defaultGateways[familyKey]; exists {
				continue
			}
			defaultResult, err := lookupDefault(ctx, ipv6, outbound.Interface)
			if err != nil {
				return defaultRoutePlan{}, fmt.Errorf("prove outbound %q physical default route on %s: %w", name, outbound.Interface, err)
			}
			if defaultResult.Interface != outbound.Interface {
				return defaultRoutePlan{}, fmt.Errorf("outbound %q physical default resolves on %q, want %q", name, defaultResult.Interface, outbound.Interface)
			}
			physicalGateway, err := parseUsableGateway(defaultResult.Gateway, probeAddress)
			if err != nil {
				return defaultRoutePlan{}, fmt.Errorf("outbound %q physical default on %s: %w", name, outbound.Interface, err)
			}
			defaultGateways[familyKey] = physicalGateway
			if err := proveScopedEgress(ctx, lookup, ipv6, outbound.Interface, physicalGateway); err != nil {
				return defaultRoutePlan{}, fmt.Errorf("outbound %q: %w", name, err)
			}
			if err := proveGatewayBypass(ctx, lookup, owned, owners, gatewayRoutes, physicalGateway, outbound.Interface); err != nil {
				return defaultRoutePlan{}, err
			}
			prefixes := ipv4CapturePrefixes
			if ipv6 {
				prefixes = ipv6CapturePrefixes
			}
			for _, prefix := range prefixes {
				route := system.RouteState{
					Prefix: prefix.String(), Interface: outbound.Interface,
					Gateway: physicalGateway.String(), Scope: outbound.Interface,
				}
				scopedRoutes[route.Prefix+"|"+route.Scope+"|"+route.Gateway] = route
			}
		}
	}

	for _, name := range names {
		outbound := runtime.Outbounds[name]
		if outbound.Type != "direct" {
			continue
		}
		for _, endpoint := range outbound.DNS {
			address := endpoint.Addr()
			if address.Is6() && !ipv6Enabled {
				continue
			}
			familyKey := outbound.Interface + "|4"
			if address.Is6() {
				familyKey = outbound.Interface + "|6"
			}
			physicalGateway := defaultGateways[familyKey]

			result, err := lookup(ctx, address, outbound.Interface)
			if err != nil {
				return defaultRoutePlan{}, fmt.Errorf("prove outbound %q DNS bypass for %s on %s: %w", name, address, outbound.Interface, err)
			}
			if result.Interface != outbound.Interface {
				return defaultRoutePlan{}, fmt.Errorf("outbound %q DNS %s resolves on %q, want %q", name, address, result.Interface, outbound.Interface)
			}
			exactHost := exactHostDestination(result.Destination, address)
			ownedHost := ownsHostRoute(owned, address, outbound.Interface, result.Gateway)
			if exactHost && !ownedHost {
				if result.Gateway == "" || strings.HasPrefix(result.Gateway, "link#") {
					if err := claimBypassAddress(owners, address, outbound.Interface); err != nil {
						return defaultRoutePlan{}, err
					}
					continue
				}
				return defaultRoutePlan{}, fmt.Errorf("outbound %q DNS %s already has an exact gateway route; refusing to assume ownership", name, address)
			}
			if err := claimBypassAddress(owners, address, outbound.Interface); err != nil {
				return defaultRoutePlan{}, err
			}

			dnsGateway, gatewayErr := parseUsableGateway(result.Gateway, address)
			dnsRoute := system.RouteState{Prefix: hostPrefix(address).String(), Interface: outbound.Interface}
			if gatewayErr == nil {
				if err := proveGatewayBypass(ctx, lookup, owned, owners, gatewayRoutes, dnsGateway, outbound.Interface); err != nil {
					return defaultRoutePlan{}, err
				}
				if address != dnsGateway.WithZone("") {
					dnsRoute.Gateway = dnsGateway.String()
				}
			} else if result.Gateway != "" && !strings.HasPrefix(result.Gateway, "link#") {
				return defaultRoutePlan{}, fmt.Errorf("outbound %q DNS %s has unusable gateway %q on %s", name, address, result.Gateway, outbound.Interface)
			}
			if address != physicalGateway.WithZone("") {
				dnsKey := dnsRoute.Prefix + "|" + dnsRoute.Interface + "|" + dnsRoute.Gateway
				dnsRoutes[dnsKey] = dnsRoute
			}
		}
	}

	plan := defaultRoutePlan{Prefixes: append([]netip.Prefix(nil), ipv4CapturePrefixes...)}
	if ipv6Enabled {
		plan.Prefixes = append(plan.Prefixes, ipv6CapturePrefixes...)
	}
	appendSortedRoutes := func(routes map[string]system.RouteState) {
		keys := make([]string, 0, len(routes))
		for key := range routes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			plan.Bypasses = append(plan.Bypasses, routes[key])
		}
	}
	// Gateways must remain directly reachable before DNS host routes refer to
	// them, and every bypass precedes the split-default routes.
	appendSortedRoutes(gatewayRoutes)
	appendSortedRoutes(dnsRoutes)
	keys := make([]string, 0, len(scopedRoutes))
	for key := range scopedRoutes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		plan.Scoped = append(plan.Scoped, scopedRoutes[key])
	}
	return plan, nil
}

func proveScopedEgress(ctx context.Context, lookup scopedRouteLookup, ipv6 bool, interfaceName string, gateway netip.Addr) error {
	candidates := []netip.Addr{
		netip.MustParseAddr("192.0.2.254"),
		netip.MustParseAddr("198.51.100.254"),
		netip.MustParseAddr("203.0.113.254"),
	}
	if ipv6 {
		candidates = []netip.Addr{
			netip.MustParseAddr("2001:db8::fffe"),
			netip.MustParseAddr("2001:db8:1::fffe"),
			netip.MustParseAddr("2001:db8:2::fffe"),
		}
	}
	for _, candidate := range candidates {
		result, err := lookup(ctx, candidate, interfaceName)
		if err != nil || result.Interface != interfaceName {
			continue
		}
		candidateGateway, err := parseUsableGateway(result.Gateway, candidate)
		if err == nil && candidateGateway == gateway && scopedEgressDestination(result, candidate) {
			return nil
		}
	}
	return fmt.Errorf("cannot prove loop-free scoped %s egress on %s through gateway %s", map[bool]string{false: "IPv4", true: "IPv6"}[ipv6], interfaceName, gateway)
}

func scopedEgressDestination(result system.RouteLookup, candidate netip.Addr) bool {
	if defaultDestination(result.Destination) && (result.Mask == "" || result.Mask == "default" || result.Mask == "0.0.0.0" || result.Mask == "::") {
		return true
	}
	if candidate.Is4() {
		return result.Mask == "128.0.0.0"
	}
	return result.Mask == "8000::"
}

func defaultDestination(value string) bool {
	return value == "default" || value == "0.0.0.0" || value == "::" || value == "0.0.0.0/0" || value == "::/0"
}

func parseUsableGateway(raw string, destination netip.Addr) (netip.Addr, error) {
	gateway, err := netip.ParseAddr(raw)
	if err != nil || gateway.IsUnspecified() || gateway.IsLoopback() || gateway.Is4In6() || gateway.Is4() != destination.Is4() {
		return netip.Addr{}, fmt.Errorf("unusable gateway %q for %s", raw, destination)
	}
	return gateway, nil
}

func addGatewayBypass(owners map[netip.Addr]string, routes map[string]system.RouteState, gateway netip.Addr, interfaceName string) error {
	address := gateway.WithZone("")
	if err := claimBypassAddress(owners, gateway, interfaceName); err != nil {
		return err
	}
	prefix := hostPrefix(address)
	key := prefix.String() + "|" + interfaceName
	routes[key] = system.RouteState{Prefix: prefix.String(), Interface: interfaceName}
	return nil
}

func proveGatewayBypass(ctx context.Context, lookup scopedRouteLookup, owned []system.RouteState, owners map[netip.Addr]string, routes map[string]system.RouteState, gateway netip.Addr, interfaceName string) error {
	address := gateway.WithZone("")
	result, err := lookup(ctx, address, interfaceName)
	if err != nil {
		return fmt.Errorf("prove gateway bypass for %s on %s: %w", address, interfaceName, err)
	}
	if result.Interface != interfaceName {
		return fmt.Errorf("gateway %s resolves on %q, want %q", address, result.Interface, interfaceName)
	}
	if exactHostDestination(result.Destination, address) {
		if ownsHostRoute(owned, address, interfaceName, "") {
			return addGatewayBypass(owners, routes, gateway, interfaceName)
		}
		if result.Gateway == "" || strings.HasPrefix(result.Gateway, "link#") {
			// macOS maintains cloned LLINFO/ROUTER host routes for an active
			// on-link gateway. Reuse that system-owned route, but never record
			// it as ours or remove it during cleanup.
			return claimBypassAddress(owners, gateway, interfaceName)
		}
		return fmt.Errorf("gateway %s already has an exact routed host entry; refusing to assume ownership", address)
	}
	return addGatewayBypass(owners, routes, gateway, interfaceName)
}

func ownsHostRoute(routes []system.RouteState, address netip.Addr, interfaceName, gateway string) bool {
	prefix := hostPrefix(address).String()
	for _, route := range routes {
		if route.Prefix != prefix || route.Interface != interfaceName {
			continue
		}
		if route.Gateway == "" || route.Gateway == gateway {
			return true
		}
	}
	return false
}

func exactHostDestination(destination string, address netip.Addr) bool {
	if parsed, err := netip.ParseAddr(destination); err == nil {
		return parsed.WithZone("") == address.WithZone("")
	}
	if prefix, err := netip.ParsePrefix(destination); err == nil {
		bits := 128
		if address.Is4() {
			bits = 32
		}
		return prefix.Bits() == bits && prefix.Addr().WithZone("") == address.WithZone("")
	}
	return false
}

func claimBypassAddress(owners map[netip.Addr]string, address netip.Addr, interfaceName string) error {
	if previous, exists := owners[address]; exists && previous != interfaceName {
		return fmt.Errorf("bypass address %s is required by both %q and %q; route ownership is ambiguous", address, previous, interfaceName)
	}
	owners[address] = interfaceName
	return nil
}

func hostPrefix(address netip.Addr) netip.Prefix {
	bits := 128
	if address.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(address, bits)
}

func (plan defaultRoutePlan) routes(tunName string) []system.RouteState {
	routes := append([]system.RouteState(nil), plan.Bypasses...)
	routes = append(routes, plan.Scoped...)
	for _, prefix := range plan.Prefixes {
		routes = append(routes, system.RouteState{Prefix: prefix.String(), Interface: tunName})
	}
	return routes
}

func (plan defaultRoutePlan) equal(other defaultRoutePlan) bool {
	if len(plan.Bypasses) != len(other.Bypasses) || len(plan.Scoped) != len(other.Scoped) || len(plan.Prefixes) != len(other.Prefixes) {
		return false
	}
	for index := range plan.Bypasses {
		if plan.Bypasses[index] != other.Bypasses[index] {
			return false
		}
	}
	for index := range plan.Scoped {
		if plan.Scoped[index] != other.Scoped[index] {
			return false
		}
	}
	for index := range plan.Prefixes {
		if plan.Prefixes[index] != other.Prefixes[index] {
			return false
		}
	}
	return true
}
