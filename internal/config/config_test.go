package config

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `version: 1
log:
  level: info
  format: text
system:
  state_file: /tmp/tun-proxy/state.json
  lock_file: /tmp/tun-proxy/lock
  manage_dns: true
  restore_on_exit: true
tun:
  address: 10.255.0.2
  peer: 10.255.0.1
  mtu: 1400
fake_ip:
  cidr: 198.18.0.0/15
  dns_ttl: 1m
  mapping_ttl: 24h
  persistence_file: /tmp/tun-proxy/fake-ip.yaml
  exclude: [localhost, "*.local"]
dns:
  listen: 127.0.0.1:5353
  udp: true
  tcp: true
  default_outbound: primary
sessions:
  udp_idle_timeout: 30s
  max_udp_sessions: 100
  max_udp_sessions_per_source: 10
outbounds:
  primary:
    type: direct
    interface: en0
    dns_source: dhcp
    dns: ["1.1.1.1:53"]
    connect_timeout: 5s
    fallback: reject
  secondary:
    type: direct
    interface: en7
    dns_source: static
    dns: ["9.9.9.9:53"]
    connect_timeout: 5s
    fallback: primary
  reject:
    type: reject
rules:
  - domain: [Example.COM.]
    protocol: tcp
    dst_port: [443]
    outbound: secondary
  - outbound: primary
`

func TestDecodeValidConfig(t *testing.T) {
	config, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.FakeIP.Prefix.String(), "198.18.0.0/15"; got != want {
		t.Fatalf("FakeIP.Prefix = %q, want %q", got, want)
	}
	if got, want := config.FakeIP.DNSTTL, time.Minute; got != want {
		t.Fatalf("FakeIP.DNSTTL = %s, want %s", got, want)
	}
	if got, want := config.Rules[0].Domains[0], "example.com"; got != want {
		t.Fatalf("normalized domain = %q, want %q", got, want)
	}
	if !config.Rules[len(config.Rules)-1].Default() {
		t.Fatal("last rule is not the default")
	}
	if config.Sessions.MaxTCPFlows != 1024 || config.Sessions.UDPIdleTimeout != 30*time.Second || config.Sessions.MaxUDPSessions != 100 || config.Sessions.MaxUDPSessionsPerSource != 10 {
		t.Fatalf("sessions = %+v", config.Sessions)
	}
	if config.TUN.PacketQueue != 1024 || config.TUN.BufferPool != 128 || config.DNS.MaxConcurrent != 256 {
		t.Fatalf("resource defaults were not applied: tun=%+v dns=%+v", config.TUN, config.DNS)
	}
	if config.Outbounds["primary"].DNSSource != DNSSourceDHCP || config.Outbounds["secondary"].DNSSource != DNSSourceStatic {
		t.Fatalf("DNS sources = primary:%q secondary:%q", config.Outbounds["primary"].DNSSource, config.Outbounds["secondary"].DNSSource)
	}
}

func TestLoadBytesWithDigestUsesExactPayload(t *testing.T) {
	contents := []byte(validYAML)
	configuration, digest, err := LoadBytesWithDigest(contents)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.DNS.Listen.String() != "127.0.0.1:5353" {
		t.Fatalf("DNS.Listen = %s", configuration.DNS.Listen)
	}
	want := sha256.Sum256(contents)
	if digest != fmt.Sprintf("sha256:%x", want[:]) {
		t.Fatalf("digest = %q, want sha256:%x", digest, want[:])
	}
}

func TestLoadBytesWithDigestRejectsEmptyAndOversizedPayloads(t *testing.T) {
	for _, contents := range [][]byte{nil, make([]byte, maxConfigSize+1)} {
		if _, _, err := LoadBytesWithDigest(contents); err == nil {
			t.Fatalf("LoadBytesWithDigest(%d bytes) succeeded", len(contents))
		}
	}
}

func TestDecodeCompilesCanonicalIPCIDRs(t *testing.T) {
	yaml := strings.Replace(validYAML, "  - domain: [Example.COM.]", "  - ip_cidr: [192.0.2.0/24, 2001:db8::/32, 192.0.2.0/24]\n    domain: [Example.COM.]", 1)
	configuration, err := Decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")}
	if len(configuration.Rules[0].DestinationCIDRs) != len(want) {
		t.Fatalf("DestinationCIDRs = %v", configuration.Rules[0].DestinationCIDRs)
	}
	for index := range want {
		if configuration.Rules[0].DestinationCIDRs[index] != want[index] {
			t.Fatalf("DestinationCIDRs[%d] = %s, want %s", index, configuration.Rules[0].DestinationCIDRs[index], want[index])
		}
	}
}

func TestDecodeRejectsInvalidIPCIDRs(t *testing.T) {
	for _, test := range []struct {
		value   string
		wantErr string
	}{
		{value: "not-a-prefix", wantErr: "invalid prefix"},
		{value: "192.0.2.1/24", wantErr: "not canonical"},
		{value: "::ffff:192.0.2.0/120", wantErr: "invalid prefix"},
	} {
		yaml := strings.Replace(validYAML, "  - domain: [Example.COM.]", "  - ip_cidr: [\""+test.value+"\"]\n    domain: [Example.COM.]", 1)
		_, err := Decode(strings.NewReader(yaml))
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Fatalf("Decode(%q) = %v, want containing %q", test.value, err, test.wantErr)
		}
	}
}

func TestDecodeDefaultRouteCaptureIsExplicitOptIn(t *testing.T) {
	configuration, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Capture.DefaultRoute {
		t.Fatal("omitted capture.default_route was enabled")
	}
	yaml := strings.Replace(validYAML, "tun:\n", "capture:\n  default_route: true\ntun:\n", 1)
	configuration, err = Decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.Capture.DefaultRoute {
		t.Fatal("capture.default_route opt-in was not compiled")
	}
}

func TestDecodeAcceptsIPv6DNSUpstream(t *testing.T) {
	yaml := strings.Replace(validYAML, `dns: ["1.1.1.1:53"]`, `dns: ["[2606:4700:4700::1111]:53"]`, 1)
	configuration, err := Decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	server := configuration.Outbounds["primary"].DNS[0]
	if !server.Addr().Is6() || server.String() != "[2606:4700:4700::1111]:53" {
		t.Fatalf("IPv6 DNS upstream = %s", server)
	}
}

func TestDecodeOptionalFakeIPv6(t *testing.T) {
	yaml := strings.Replace(validYAML, "  peer: 10.255.0.1\n", `  peer: 10.255.0.1
  ipv6_address: fd00:7475:6e70:ffff::2
  ipv6_peer: fd00:7475:6e70:ffff::1
`, 1)
	yaml = strings.Replace(yaml, "dns:\n", `fake_ipv6:
  cidr: fd00:7::/120
  max_mappings: 128
  persistence_file: /tmp/tun-proxy/fake-ipv6.yaml
dns:
`, 1)
	configuration, err := Decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.FakeIPv6 == nil || configuration.FakeIPv6.Prefix.String() != "fd00:7::/120" || configuration.FakeIPv6.MaxMappings != 128 {
		t.Fatalf("FakeIPv6 = %+v", configuration.FakeIPv6)
	}
	if got := configuration.TUN.IPv6Address.String(); got != "fd00:7475:6e70:ffff::2" {
		t.Fatalf("TUN.IPv6Address = %q", got)
	}

	without, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if without.FakeIPv6 != nil {
		t.Fatalf("omitted fake_ipv6 = %+v", without.FakeIPv6)
	}
}

func TestDecodeRejectsInvalidFakeIPv6(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		wantErr string
	}{
		{name: "missing prefix", block: "fake_ipv6: {}\n", wantErr: "cidr is required"},
		{name: "IPv4 prefix", block: "fake_ipv6:\n  cidr: 198.19.0.0/16\n", wantErr: "must be an IPv6 prefix"},
		{name: "public prefix", block: "fake_ipv6:\n  cidr: 2001:db8::/96\n", wantErr: "unique-local"},
		{name: "too small", block: "fake_ipv6:\n  cidr: fd00:7::/126\n", wantErr: "no usable"},
		{name: "limit exceeds capacity", block: "fake_ipv6:\n  cidr: fd00:7::/124\n  max_mappings: 6\n", wantErr: "max_mappings"},
		{name: "shared persistence", block: "fake_ipv6:\n  cidr: fd00:7::/96\n  persistence_file: /tmp/tun-proxy/fake-ip.yaml\n", wantErr: "must differ"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			yaml := strings.Replace(validYAML, "dns:\n", test.block+"dns:\n", 1)
			_, err := Decode(strings.NewReader(yaml))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Decode() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeRejectsIncompleteOrUnsafeTUNIPv6(t *testing.T) {
	tests := []struct {
		name    string
		tun     string
		fake    string
		wantErr string
	}{
		{name: "address only", tun: "  ipv6_address: fd00:ffff::2\n", wantErr: "must be configured together"},
		{name: "without pool", tun: "  ipv6_address: fd00:ffff::2\n  ipv6_peer: fd00:ffff::1\n", wantErr: "require fake_ipv6"},
		{name: "same addresses", tun: "  ipv6_address: fd00:ffff::1\n  ipv6_peer: fd00:ffff::1\n", fake: "fake_ipv6:\n  cidr: fd00:7::/96\n", wantErr: "must differ"},
		{name: "inside fake prefix", tun: "  ipv6_address: fd00:7::12\n  ipv6_peer: fd00:ffff::1\n", fake: "fake_ipv6:\n  cidr: fd00:7::/96\n", wantErr: "must not contain"},
		{name: "mapped IPv4", tun: "  ipv6_address: ::ffff:10.0.0.2\n  ipv6_peer: fd00:ffff::1\n", fake: "fake_ipv6:\n  cidr: fd00:7::/96\n", wantErr: "unicast IPv6"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			yaml := strings.Replace(validYAML, "  peer: 10.255.0.1\n", "  peer: 10.255.0.1\n"+test.tun, 1)
			if test.fake != "" {
				yaml = strings.Replace(yaml, "dns:\n", test.fake+"dns:\n", 1)
			}
			_, err := Decode(strings.NewReader(yaml))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Decode() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeRejectsUnsafeDNSUpstream(t *testing.T) {
	tests := []string{
		"127.0.0.1:53",
		"0.0.0.0:53",
		"[::1]:53",
		"[::]:53",
		"[::ffff:1.1.1.1]:53",
		"1.1.1.1:0",
	}
	for _, server := range tests {
		t.Run(server, func(t *testing.T) {
			yaml := strings.Replace(validYAML, `dns: ["1.1.1.1:53"]`, `dns: ["`+server+`"]`, 1)
			_, err := Decode(strings.NewReader(yaml))
			if err == nil || !strings.Contains(err.Error(), "non-loopback IPv4 or IPv6") {
				t.Fatalf("Decode() error = %v", err)
			}
		})
	}
}

func TestLoadExampleConfig(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "unknown field", yaml: strings.Replace(validYAML, "  format: text", "  format: text\n  colour: true", 1), wantErr: "field colour not found"},
		{name: "duplicate field", yaml: strings.Replace(validYAML, "version: 1", "version: 1\nversion: 1", 1), wantErr: "mapping key \"version\" already defined"},
		{name: "unsupported version", yaml: strings.Replace(validYAML, "version: 1", "version: 2", 1), wantErr: "version must be 1"},
		{name: "rollback disabled", yaml: strings.Replace(validYAML, "restore_on_exit: true", "restore_on_exit: false", 1), wantErr: "host network rollback is mandatory"},
		{name: "bad duration", yaml: strings.Replace(validYAML, "dns_ttl: 1m", "dns_ttl: soon", 1), wantErr: "positive duration"},
		{name: "bad UDP idle timeout", yaml: strings.Replace(validYAML, "udp_idle_timeout: 30s", "udp_idle_timeout: never", 1), wantErr: "sessions.udp_idle_timeout"},
		{name: "bad TCP limit", yaml: strings.Replace(validYAML, "sessions:\n", "sessions:\n  max_tcp_flows: -1\n", 1), wantErr: "sessions.max_tcp_flows"},
		{name: "bad DNS limit", yaml: strings.Replace(validYAML, "default_outbound: primary", "default_outbound: primary\n  max_concurrent: 70000", 1), wantErr: "dns.max_concurrent"},
		{name: "bad packet queue", yaml: strings.Replace(validYAML, "mtu: 1400", "mtu: 1400\n  packet_queue: 1", 1), wantErr: "tun.packet_queue"},
		{name: "per-source over total", yaml: strings.Replace(validYAML, "max_udp_sessions_per_source: 10", "max_udp_sessions_per_source: 101", 1), wantErr: "must be between 1 and sessions.max_udp_sessions"},
		{name: "mapping ttl too short", yaml: strings.Replace(validYAML, "mapping_ttl: 24h", "mapping_ttl: 30s", 1), wantErr: "mapping_ttl must be greater"},
		{name: "non-loopback listener", yaml: strings.Replace(validYAML, "127.0.0.1:5353", "0.0.0.0:5353", 1), wantErr: "must be loopback"},
		{name: "undefined default outbound", yaml: strings.Replace(validYAML, "default_outbound: primary", "default_outbound: missing", 1), wantErr: "is not defined"},
		{name: "undefined rule outbound", yaml: strings.Replace(validYAML, "outbound: secondary", "outbound: missing", 1), wantErr: "undefined outbound"},
		{name: "unsupported DNS source", yaml: strings.Replace(validYAML, "dns_source: dhcp", "dns_source: auto", 1), wantErr: "dns_source must be \"dhcp\" or \"static\""},
		{name: "missing DHCP DNS fallback", yaml: strings.Replace(validYAML, `    dns: ["1.1.1.1:53"]`, "    dns: []", 1), wantErr: "must configure at least one DNS server"},
		{name: "missing static DNS list", yaml: strings.Replace(validYAML, `    dns: ["9.9.9.9:53"]`, "    dns: []", 1), wantErr: "must configure at least one DNS server"},
		{name: "reject DNS source", yaml: strings.Replace(validYAML, "  reject:\n    type: reject", "  reject:\n    type: reject\n    dns_source: static", 1), wantErr: "cannot set interface, dns_source, dns"},
		{name: "invalid port", yaml: strings.Replace(validYAML, "dst_port: [443]", "dst_port: [0]", 1), wantErr: "outside 1..65535"},
		{name: "default not last", yaml: strings.Replace(validYAML, "rules:\n", "rules:\n  - outbound: primary\n", 1), wantErr: "must be last"},
		{name: "fallback cycle", yaml: strings.Replace(validYAML, "fallback: reject", "fallback: secondary", 1), wantErr: "fallback cycle"},
		{name: "multiple documents", yaml: validYAML + "---\nversion: 1\n", wantErr: "multiple documents"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Decode() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeAppliesDefaults(t *testing.T) {
	minimal := `version: 1
dns:
  default_outbound: primary
outbounds:
  primary:
    type: direct
    interface: en0
    dns: ["1.1.1.1:53"]
rules:
  - outbound: primary
`
	config, err := Decode(strings.NewReader(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if config.TUN.MTU != 1400 || config.FakeIP.DNSTTL != time.Minute || !config.DNS.UDP || !config.DNS.TCP {
		t.Fatalf("defaults were not applied: %+v", config)
	}
	if got := config.Outbounds["primary"].DNSSource; got != DNSSourceDHCP {
		t.Fatalf("default DNS source = %q, want %q", got, DNSSourceDHCP)
	}
	if len(config.FakeIP.Exclude) != 3 {
		t.Fatalf("default excludes = %v, want 3 entries", config.FakeIP.Exclude)
	}
	if config.Sessions.MaxTCPFlows != 1024 || config.Sessions.UDPIdleTimeout != 2*time.Minute || config.Sessions.MaxUDPSessions != 4096 || config.Sessions.MaxUDPSessionsPerSource != 256 {
		t.Fatalf("session defaults = %+v", config.Sessions)
	}
}
