//go:build darwin

package system

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const ipconfig = "/usr/sbin/ipconfig"

// DiscoverInterfaceDNS reads the DNS servers carried in each interface's
// current DHCP lease. Interfaces without a DHCP lease or without DHCP DNS are
// omitted so callers can fall back to their configured upstreams.
func DiscoverInterfaceDNS(ctx context.Context, runner CommandRunner, interfaces []string) (map[string][]netip.AddrPort, error) {
	result := make(map[string][]netip.AddrPort)
	names := append([]string(nil), interfaces...)
	slices.Sort(names)
	names = slices.Compact(names)
	for _, interfaceName := range names {
		if err := validateInterfaceName(interfaceName); err != nil {
			return result, err
		}
		output, err := runner.Run(ctx, ipconfig, "getpacket", interfaceName)
		if err != nil {
			// A non-DHCP or temporarily disconnected interface normally has no
			// packet. Treat that as "no dynamic DNS" and let configuration win.
			continue
		}
		servers, err := parseDHCPDNS(string(output))
		if err != nil {
			return result, fmt.Errorf("parse DHCP DNS for interface %q: %w", interfaceName, err)
		}
		if len(servers) != 0 {
			result[interfaceName] = servers
		}
	}
	return result, nil
}

func parseDHCPDNS(output string) ([]netip.AddrPort, error) {
	const marker = "domain_name_server (ip_mult):"
	var servers []netip.AddrPort
	seen := make(map[netip.Addr]struct{})
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, marker) {
			continue
		}
		values := strings.TrimSpace(strings.TrimPrefix(line, marker))
		values = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(values, "{"), "}"))
		if values == "" {
			continue
		}
		for _, raw := range strings.Split(values, ",") {
			address, err := netip.ParseAddr(strings.TrimSpace(raw))
			if err != nil {
				return nil, fmt.Errorf("invalid DHCP DNS address %q", strings.TrimSpace(raw))
			}
			address = address.Unmap()
			if !usableUpstreamDNS(address) {
				continue
			}
			if _, exists := seen[address]; exists {
				continue
			}
			seen[address] = struct{}{}
			servers = append(servers, netip.AddrPortFrom(address, 53))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}

func usableUpstreamDNS(address netip.Addr) bool {
	return address.IsValid() && !address.IsUnspecified() && !address.IsLoopback() && !address.IsMulticast()
}

func validateInterfaceName(name string) error {
	if name == "" || len(name) > 15 || strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid network interface name %q", name)
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
			continue
		}
		return fmt.Errorf("invalid network interface name %q", name)
	}
	return nil
}
