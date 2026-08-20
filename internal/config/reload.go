package config

import (
	"errors"
	"fmt"
	"reflect"
)

// ValidateReload rejects fields whose live replacement would invalidate host
// state or the shared Fake IP pool. All other compiled fields may be applied to
// newly created DNS queries and transport sessions.
func ValidateReload(current, next *Config) error {
	if current == nil || next == nil {
		return errors.New("current and next configuration are required")
	}
	var failures []error
	requireEqual := func(name string, left, right any) {
		if left != right {
			failures = append(failures, fmt.Errorf("%s cannot be reloaded (current %v, next %v)", name, left, right))
		}
	}
	requireEqual("version", current.Version, next.Version)
	requireEqual("system.state_file", current.System.StateFile, next.System.StateFile)
	requireEqual("system.lock_file", current.System.LockFile, next.System.LockFile)
	requireEqual("system.manage_dns", current.System.ManageDNS, next.System.ManageDNS)
	requireEqual("capture.default_route", current.Capture.DefaultRoute, next.Capture.DefaultRoute)
	requireEqual("tun.address", current.TUN.Address, next.TUN.Address)
	requireEqual("tun.peer", current.TUN.Peer, next.TUN.Peer)
	requireEqual("tun.ipv6_address", current.TUN.IPv6Address, next.TUN.IPv6Address)
	requireEqual("tun.ipv6_peer", current.TUN.IPv6Peer, next.TUN.IPv6Peer)
	requireEqual("tun.mtu", current.TUN.MTU, next.TUN.MTU)
	requireEqual("tun.packet_queue", current.TUN.PacketQueue, next.TUN.PacketQueue)
	requireEqual("tun.buffer_pool", current.TUN.BufferPool, next.TUN.BufferPool)
	requireEqual("fake_ip.cidr", current.FakeIP.Prefix, next.FakeIP.Prefix)
	requireEqual("fake_ip.mapping_ttl", current.FakeIP.MappingTTL, next.FakeIP.MappingTTL)
	requireEqual("fake_ip.max_mappings", current.FakeIP.MaxMappings, next.FakeIP.MaxMappings)
	requireEqual("fake_ip.persistence_file", current.FakeIP.PersistenceFile, next.FakeIP.PersistenceFile)
	if (current.FakeIPv6 == nil) != (next.FakeIPv6 == nil) {
		failures = append(failures, errors.New("fake_ipv6 cannot be enabled or disabled by reload"))
	} else if current.FakeIPv6 != nil {
		requireEqual("fake_ipv6.cidr", current.FakeIPv6.Prefix, next.FakeIPv6.Prefix)
		requireEqual("fake_ipv6.max_mappings", current.FakeIPv6.MaxMappings, next.FakeIPv6.MaxMappings)
		requireEqual("fake_ipv6.persistence_file", current.FakeIPv6.PersistenceFile, next.FakeIPv6.PersistenceFile)
	}
	requireEqual("dns.listen", current.DNS.Listen, next.DNS.Listen)
	requireEqual("dns.udp", current.DNS.UDP, next.DNS.UDP)
	requireEqual("dns.tcp", current.DNS.TCP, next.DNS.TCP)
	requireEqual("dns.max_concurrent", current.DNS.MaxConcurrent, next.DNS.MaxConcurrent)
	requireEqual("sessions.max_tcp_flows", current.Sessions.MaxTCPFlows, next.Sessions.MaxTCPFlows)
	requireEqual("sessions.max_udp_sessions", current.Sessions.MaxUDPSessions, next.Sessions.MaxUDPSessions)
	requireEqual("sessions.max_udp_sessions_per_source", current.Sessions.MaxUDPSessionsPerSource, next.Sessions.MaxUDPSessionsPerSource)
	if current.Capture.DefaultRoute && !reflect.DeepEqual(current.Outbounds, next.Outbounds) {
		failures = append(failures, errors.New("outbounds cannot be reloaded while capture.default_route is enabled"))
	}
	return errors.Join(failures...)
}
