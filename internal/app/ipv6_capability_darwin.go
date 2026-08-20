//go:build darwin

package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/interfaceinfo"
	"github.com/hailinpan/tun-proxy/internal/system"
)

// IPv6DataPathAvailable reports whether the configured physical outbounds can
// currently carry general IPv6 traffic. A configured Fake IPv6 pool remains
// prepared when this returns false, but Fake AAAA must stay disabled until the
// process is restarted on an IPv6-capable network.
func IPv6DataPathAvailable(ctx context.Context, runtime *config.Config) (bool, string) {
	return ipv6DataPathAvailable(ctx, runtime, interfaceinfo.Get, system.LookupDefaultRoute)
}

type interfaceGetter func(string) (interfaceinfo.Interface, error)
type familyDefaultRouteLookup func(context.Context, bool) (system.RouteLookup, error)

func ipv6DataPathAvailable(ctx context.Context, runtime *config.Config, getInterface interfaceGetter, lookupDefault familyDefaultRouteLookup) (bool, string) {
	if runtime == nil || runtime.FakeIPv6 == nil {
		return false, "fake_ipv6 is not configured"
	}
	capable := make(map[string]struct{})
	checked := make(map[string]struct{})
	for _, outbound := range runtime.Outbounds {
		if outbound.Type != "direct" {
			continue
		}
		if _, exists := checked[outbound.Interface]; exists {
			continue
		}
		checked[outbound.Interface] = struct{}{}
		iface, err := getInterface(outbound.Interface)
		if err != nil {
			continue
		}
		for _, raw := range iface.Addresses {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				continue
			}
			address := prefix.Addr()
			if address.Is6() && !address.Is4In6() && address.IsGlobalUnicast() && !address.IsLinkLocalUnicast() {
				capable[outbound.Interface] = struct{}{}
				break
			}
		}
	}
	if len(capable) == 0 {
		return false, "configured outbound interfaces have no usable non-link-local IPv6 address"
	}
	lookup, err := lookupDefault(ctx, true)
	if errors.Is(err, system.ErrRouteNotFound) {
		return false, "system has no IPv6 default route"
	}
	if err != nil {
		return false, fmt.Sprintf("inspect IPv6 default route: %v", err)
	}
	if !ipv6DefaultDestination(lookup.Destination) {
		return false, fmt.Sprintf("IPv6 default lookup returned non-default destination %q", lookup.Destination)
	}
	if _, exists := capable[lookup.Interface]; !exists {
		return false, fmt.Sprintf("IPv6 default route uses unconfigured or IPv6-incapable interface %q", lookup.Interface)
	}
	return true, ""
}

func ipv6DefaultDestination(destination string) bool {
	destination = strings.TrimSpace(destination)
	return destination == "default" || destination == "::" || destination == "::/0"
}
