//go:build darwin

package app

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/system"
)

type interfaceDNS map[string][]netip.AddrPort

func discoverInterfaceDNS(ctx context.Context, runtime *config.Config, runner system.CommandRunner) (interfaceDNS, error) {
	interfaces := make([]string, 0, len(runtime.Outbounds))
	for _, outbound := range runtime.Outbounds {
		if outbound.Type == "direct" && outbound.DNSSource == config.DNSSourceDHCP {
			interfaces = append(interfaces, outbound.Interface)
		}
	}
	return system.DiscoverInterfaceDNS(ctx, runner, interfaces)
}

// runtimeWithInterfaceDNS returns a shallow runtime copy whose outbound map
// contains independent values. DHCP-mode direct outbounds use a non-empty
// discovered list; static-mode outbounds and DHCP misses retain validated YAML.
func runtimeWithInterfaceDNS(runtime *config.Config, discovered interfaceDNS) *config.Config {
	if runtime == nil {
		return nil
	}
	effective := *runtime
	effective.Outbounds = make(map[string]config.Outbound, len(runtime.Outbounds))
	for name, outbound := range runtime.Outbounds {
		if outbound.Type == "direct" && outbound.DNSSource == config.DNSSourceDHCP && len(discovered[outbound.Interface]) != 0 {
			outbound.DNS = append([]netip.AddrPort(nil), discovered[outbound.Interface]...)
		} else {
			outbound.DNS = append([]netip.AddrPort(nil), outbound.DNS...)
		}
		effective.Outbounds[name] = outbound
	}
	return &effective
}

func sameInterfaceDNS(left, right interfaceDNS) bool {
	if len(left) != len(right) {
		return false
	}
	for interfaceName, servers := range left {
		if !slices.Equal(servers, right[interfaceName]) {
			return false
		}
	}
	return true
}

func effectiveDNSMessages(runtime *config.Config, discovered interfaceDNS) []string {
	names := make([]string, 0, len(runtime.Outbounds))
	for name, outbound := range runtime.Outbounds {
		if outbound.Type == "direct" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	messages := make([]string, 0, len(names))
	for _, name := range names {
		outbound := runtime.Outbounds[name]
		servers := outbound.DNS
		source := config.DNSSourceStatic
		if outbound.DNSSource == config.DNSSourceDHCP && len(discovered[outbound.Interface]) != 0 {
			servers = discovered[outbound.Interface]
			source = config.DNSSourceDHCP
		} else if outbound.DNSSource == config.DNSSourceDHCP {
			source = "configured-fallback"
		}
		values := make([]string, 0, len(servers))
		for _, server := range servers {
			values = append(values, server.String())
		}
		messages = append(messages, fmt.Sprintf(
			"effective DNS outbound=%s interface=%s source=%s servers=%s",
			name, outbound.Interface, source, strings.Join(values, ","),
		))
	}
	return messages
}
