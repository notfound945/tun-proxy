//go:build darwin

package session

import (
	"fmt"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/hailinpan/tun-proxy/internal/resolver"
)

const resolverMaxConcurrent = 64

// BuildRoutes turns validated configuration into per-outbound resolver and
// dialer instances. Resolver caches are therefore isolated by construction.
func BuildRoutes(configured map[string]config.Outbound) (map[string]Route, error) {
	routes := make(map[string]Route, len(configured))
	for name, item := range configured {
		if item.Type == "reject" {
			routes[name] = Route{Name: name, Reject: true}
			continue
		}
		boundResolver, err := resolver.NewClient(item.Interface, item.DNS, item.ConnectTimeout, resolverMaxConcurrent)
		if err != nil {
			return nil, fmt.Errorf("build resolver for outbound %q: %w", name, err)
		}
		dialer, err := outbound.NewDirect(name, item.Interface, item.ConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("build dialer for outbound %q: %w", name, err)
		}
		routes[name] = Route{
			Name: name, Resolver: boundResolver, Dialer: dialer, PacketDialer: dialer, Fallback: item.Fallback,
		}
	}
	return routes, nil
}
