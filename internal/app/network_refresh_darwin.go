//go:build darwin

package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/fakedns"
	"github.com/hailinpan/tun-proxy/internal/interfaceinfo"
	"github.com/hailinpan/tun-proxy/internal/resolver"
)

func networkFingerprint(runtime *config.Config) string {
	names := make([]string, 0, len(runtime.Outbounds))
	seen := make(map[string]struct{})
	for _, outbound := range runtime.Outbounds {
		if outbound.Type == "direct" {
			seen[outbound.Interface] = struct{}{}
		}
	}
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		info, err := interfaceinfo.Get(name)
		if err != nil {
			parts = append(parts, name+":unavailable:"+err.Error())
			continue
		}
		addresses := append([]string(nil), info.Addresses...)
		slices.Sort(addresses)
		var dns []string
		for _, outbound := range runtime.Outbounds {
			if outbound.Type != "direct" || outbound.Interface != name {
				continue
			}
			for _, server := range outbound.DNS {
				dns = append(dns, server.String())
			}
		}
		slices.Sort(dns)
		dns = slices.Compact(dns)
		parts = append(parts, fmt.Sprintf("%s:%d:%t:%t:%s:dns=%s", name, info.Index, info.Up(), info.Running(), strings.Join(addresses, ","), strings.Join(dns, ",")))
	}
	return strings.Join(parts, "|")
}

func refreshNetwork(runtime *config.Config, plane *dataPlane, dnsServer *fakedns.Server) error {
	if err := checkInterfaces(runtime); err != nil {
		return err
	}
	nextGeneration, err := plane.prepare(runtime)
	if err != nil {
		return fmt.Errorf("rebuild data plane after network change: %w", err)
	}
	defaultOutbound := runtime.Outbounds[runtime.DNS.DefaultOutbound]
	nextResolver, err := resolver.NewClient(defaultOutbound.Interface, defaultOutbound.DNS, runtimeDNSQueryTimeout, runtime.DNS.MaxConcurrent)
	if err != nil {
		return fmt.Errorf("rebuild Fake DNS resolver after network change: %w", err)
	}
	if err := dnsServer.Reload(runtime.FakeIP.DNSTTL, runtimeDNSQueryTimeout, runtime.FakeIP.Exclude, nextResolver); err != nil {
		return err
	}
	plane.commit(nextGeneration)
	return nil
}
