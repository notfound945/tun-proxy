// Package config strictly decodes user YAML and compiles it into validated,
// strongly typed runtime configuration.
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/domainname"
	"go.yaml.in/yaml/v3"
)

const CurrentVersion = 1
const maxConfigSize = 1 << 20

const (
	DNSSourceDHCP   = "dhcp"
	DNSSourceStatic = "static"
)

type Config struct {
	Version   int
	Log       Log
	System    System
	Capture   Capture
	TUN       TUN
	FakeIP    FakeIP
	FakeIPv6  *FakeIPv6
	DNS       DNS
	Sessions  Sessions
	Outbounds map[string]Outbound
	Rules     []Rule
}

type Capture struct {
	DefaultRoute bool
}

type Log struct {
	Level  string
	Format string
}

type System struct {
	StateFile string
	LockFile  string
	ManageDNS bool
}

type TUN struct {
	Address     netip.Addr
	Peer        netip.Addr
	IPv6Address netip.Addr
	IPv6Peer    netip.Addr
	MTU         int
	PacketQueue int
	BufferPool  int
}

type FakeIP struct {
	Prefix          netip.Prefix
	DNSTTL          time.Duration
	MappingTTL      time.Duration
	MaxMappings     int
	PersistenceFile string
	Exclude         []DomainPattern
}

type FakeIPv6 struct {
	Prefix          netip.Prefix
	MaxMappings     int
	PersistenceFile string
}

type DNS struct {
	Listen          netip.AddrPort
	UDP             bool
	TCP             bool
	DefaultOutbound string
	MaxConcurrent   int
}

type Sessions struct {
	MaxTCPFlows             int
	UDPIdleTimeout          time.Duration
	MaxUDPSessions          int
	MaxUDPSessionsPerSource int
}

type Outbound struct {
	Name           string
	Type           string
	Interface      string
	DNSSource      string
	DNS            []netip.AddrPort
	ConnectTimeout time.Duration
	Fallback       string
}

type Rule struct {
	ID               int
	Domains          []string
	DomainSuffixes   []string
	DestinationCIDRs []netip.Prefix
	Outbound         string
}

func (r Rule) Default() bool {
	return len(r.Domains) == 0 && len(r.DomainSuffixes) == 0 && len(r.DestinationCIDRs) == 0
}

type DomainPattern = domainname.Pattern

type rawConfig struct {
	Version   int                    `yaml:"version"`
	Log       rawLog                 `yaml:"log"`
	System    rawSystem              `yaml:"system"`
	Capture   rawCapture             `yaml:"capture"`
	TUN       rawTUN                 `yaml:"tun"`
	FakeIP    rawFakeIP              `yaml:"fake_ip"`
	FakeIPv6  *rawFakeIPv6           `yaml:"fake_ipv6"`
	DNS       rawDNS                 `yaml:"dns"`
	Sessions  rawSessions            `yaml:"sessions"`
	Outbounds map[string]rawOutbound `yaml:"outbounds"`
	Rules     []rawRule              `yaml:"rules"`
}

type rawCapture struct {
	DefaultRoute *bool `yaml:"default_route"`
}

type rawLog struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type rawSystem struct {
	StateFile     string `yaml:"state_file"`
	LockFile      string `yaml:"lock_file"`
	ManageDNS     *bool  `yaml:"manage_dns"`
	RestoreOnExit *bool  `yaml:"restore_on_exit"`
}

type rawTUN struct {
	Address     string `yaml:"address"`
	Peer        string `yaml:"peer"`
	IPv6Address string `yaml:"ipv6_address"`
	IPv6Peer    string `yaml:"ipv6_peer"`
	MTU         int    `yaml:"mtu"`
	PacketQueue int    `yaml:"packet_queue"`
	BufferPool  int    `yaml:"buffer_pool"`
}

type rawFakeIP struct {
	CIDR            string   `yaml:"cidr"`
	DNSTTL          string   `yaml:"dns_ttl"`
	MappingTTL      string   `yaml:"mapping_ttl"`
	MaxMappings     int      `yaml:"max_mappings"`
	PersistenceFile string   `yaml:"persistence_file"`
	Exclude         []string `yaml:"exclude"`
}

type rawFakeIPv6 struct {
	CIDR            string `yaml:"cidr"`
	MaxMappings     int    `yaml:"max_mappings"`
	PersistenceFile string `yaml:"persistence_file"`
}

type rawDNS struct {
	Listen          string `yaml:"listen"`
	UDP             *bool  `yaml:"udp"`
	TCP             *bool  `yaml:"tcp"`
	DefaultOutbound string `yaml:"default_outbound"`
	MaxConcurrent   int    `yaml:"max_concurrent"`
}

type rawSessions struct {
	MaxTCPFlows             int    `yaml:"max_tcp_flows"`
	UDPIdleTimeout          string `yaml:"udp_idle_timeout"`
	MaxUDPSessions          int    `yaml:"max_udp_sessions"`
	MaxUDPSessionsPerSource int    `yaml:"max_udp_sessions_per_source"`
}

type rawOutbound struct {
	Type           string   `yaml:"type"`
	Interface      string   `yaml:"interface"`
	DNSSource      string   `yaml:"dns_source"`
	DNS            []string `yaml:"dns"`
	ConnectTimeout string   `yaml:"connect_timeout"`
	Fallback       string   `yaml:"fallback"`
}

type rawRule struct {
	Domain       []string `yaml:"domain"`
	DomainSuffix []string `yaml:"domain_suffix"`
	IPCIDR       []string `yaml:"ip_cidr"`
	Outbound     string   `yaml:"outbound"`
}

func LoadFile(path string) (*Config, error) {
	contents, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	runtime, _, err := LoadBytesWithDigest(contents)
	return runtime, err
}

func LoadFileWithDigest(path string) (*Config, string, error) {
	contents, err := readConfigFile(path)
	if err != nil {
		return nil, "", err
	}
	return LoadBytesWithDigest(contents)
}

// LoadBytesWithDigest validates a supervisor-provided configuration payload,
// compiles it, and returns the digest of the exact bytes that were compiled.
// The size limit matches LoadFile so the private worker protocol cannot bypass
// the file-based configuration boundary.
func LoadBytesWithDigest(contents []byte) (*Config, string, error) {
	if len(contents) == 0 {
		return nil, "", errors.New("config is empty")
	}
	if len(contents) > maxConfigSize {
		return nil, "", errors.New("config exceeds 1 MiB")
	}
	runtime, err := Decode(bytes.NewReader(contents))
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(contents)
	return runtime, fmt.Sprintf("sha256:%x", digest[:]), nil
}

func readConfigFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // Best-effort cleanup.
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect config %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config %q is not a regular file", path)
	}
	if info.Size() > maxConfigSize {
		return nil, fmt.Errorf("config %q exceeds 1 MiB", path)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	if len(contents) > maxConfigSize {
		return nil, fmt.Errorf("config %q exceeds 1 MiB", path)
	}
	return contents, nil
}

func DigestFile(path string) (string, error) {
	contents, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(contents)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func Decode(reader io.Reader) (*Config, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)

	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode YAML: multiple documents are not allowed")
		}
		return nil, fmt.Errorf("decode YAML trailer: %w", err)
	}

	compiled, err := compile(raw)
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return compiled, nil
}

func compile(raw rawConfig) (*Config, error) {
	if raw.Version != CurrentVersion {
		return nil, fmt.Errorf("version must be %d, got %d", CurrentVersion, raw.Version)
	}

	result := &Config{Version: raw.Version}
	var err error
	if result.Log, err = compileLog(raw.Log); err != nil {
		return nil, err
	}
	if result.System, err = compileSystem(raw.System); err != nil {
		return nil, err
	}
	result.Capture = Capture{DefaultRoute: defaultBool(raw.Capture.DefaultRoute, false)}
	if result.TUN, err = compileTUN(raw.TUN); err != nil {
		return nil, err
	}
	if result.FakeIP, err = compileFakeIP(raw.FakeIP); err != nil {
		return nil, err
	}
	if raw.FakeIPv6 != nil {
		fakeIPv6, err := compileFakeIPv6(*raw.FakeIPv6)
		if err != nil {
			return nil, err
		}
		result.FakeIPv6 = &fakeIPv6
		if result.FakeIPv6.PersistenceFile == result.FakeIP.PersistenceFile {
			return nil, errors.New("fake_ipv6.persistence_file must differ from fake_ip.persistence_file")
		}
		if !result.TUN.IPv6Address.IsValid() || !result.TUN.IPv6Peer.IsValid() {
			return nil, errors.New("tun.ipv6_address and tun.ipv6_peer are required when fake_ipv6 is configured")
		}
		if result.FakeIPv6.Prefix.Contains(result.TUN.IPv6Address) || result.FakeIPv6.Prefix.Contains(result.TUN.IPv6Peer) {
			return nil, errors.New("fake_ipv6.cidr must not contain the TUN IPv6 address or peer")
		}
	} else if result.TUN.IPv6Address.IsValid() || result.TUN.IPv6Peer.IsValid() {
		return nil, errors.New("tun.ipv6_address and tun.ipv6_peer require fake_ipv6")
	}
	if result.DNS, err = compileDNS(raw.DNS); err != nil {
		return nil, err
	}
	if result.Sessions, err = compileSessions(raw.Sessions); err != nil {
		return nil, err
	}
	if result.FakeIP.Prefix.Contains(result.TUN.Address) || result.FakeIP.Prefix.Contains(result.TUN.Peer) {
		return nil, errors.New("fake_ip.cidr must not contain the TUN address or peer")
	}
	if result.Outbounds, err = compileOutbounds(raw.Outbounds); err != nil {
		return nil, err
	}
	if _, ok := result.Outbounds[result.DNS.DefaultOutbound]; !ok {
		return nil, fmt.Errorf("dns.default_outbound %q is not defined", result.DNS.DefaultOutbound)
	}
	if result.Outbounds[result.DNS.DefaultOutbound].Type == "reject" {
		return nil, errors.New("dns.default_outbound must refer to a direct outbound")
	}
	if err := validateFallbacks(result.Outbounds); err != nil {
		return nil, err
	}
	if result.Rules, err = compileRules(raw.Rules, result.Outbounds); err != nil {
		return nil, err
	}
	return result, nil
}

func compileLog(raw rawLog) (Log, error) {
	level := defaultString(strings.ToLower(raw.Level), "info")
	format := defaultString(strings.ToLower(raw.Format), "text")
	if !oneOf(level, "debug", "info", "warn", "error") {
		return Log{}, fmt.Errorf("log.level %q is not supported", level)
	}
	if !oneOf(format, "text", "json") {
		return Log{}, fmt.Errorf("log.format %q is not supported", format)
	}
	return Log{Level: level, Format: format}, nil
}

func compileSystem(raw rawSystem) (System, error) {
	if raw.RestoreOnExit != nil && !*raw.RestoreOnExit {
		return System{}, errors.New("system.restore_on_exit must be true; host network rollback is mandatory")
	}
	result := System{
		StateFile: defaultString(raw.StateFile, "/var/run/tun-proxy/state.json"),
		LockFile:  defaultString(raw.LockFile, "/var/run/tun-proxy/tun-proxy.lock"),
		ManageDNS: defaultBool(raw.ManageDNS, true),
	}
	for name, path := range map[string]string{"system.state_file": result.StateFile, "system.lock_file": result.LockFile} {
		if err := validateAbsolutePath(name, path); err != nil {
			return System{}, err
		}
	}
	if result.StateFile == result.LockFile {
		return System{}, errors.New("system.state_file and system.lock_file must differ")
	}
	return result, nil
}

func compileTUN(raw rawTUN) (TUN, error) {
	address, err := parseIPv4("tun.address", defaultString(raw.Address, "10.255.0.2"))
	if err != nil {
		return TUN{}, err
	}
	peer, err := parseIPv4("tun.peer", defaultString(raw.Peer, "10.255.0.1"))
	if err != nil {
		return TUN{}, err
	}
	if address == peer {
		return TUN{}, errors.New("tun.address and tun.peer must differ")
	}
	var ipv6Address, ipv6Peer netip.Addr
	if raw.IPv6Address != "" || raw.IPv6Peer != "" {
		if raw.IPv6Address == "" || raw.IPv6Peer == "" {
			return TUN{}, errors.New("tun.ipv6_address and tun.ipv6_peer must be configured together")
		}
		if ipv6Address, err = parseIPv6("tun.ipv6_address", raw.IPv6Address); err != nil {
			return TUN{}, err
		}
		if ipv6Peer, err = parseIPv6("tun.ipv6_peer", raw.IPv6Peer); err != nil {
			return TUN{}, err
		}
		if ipv6Address == ipv6Peer {
			return TUN{}, errors.New("tun.ipv6_address and tun.ipv6_peer must differ")
		}
	}
	mtu := raw.MTU
	if mtu == 0 {
		mtu = 1400
	}
	if mtu < 576 || mtu > 9000 {
		return TUN{}, fmt.Errorf("tun.mtu must be between 576 and 9000, got %d", mtu)
	}
	packetQueue := raw.PacketQueue
	if packetQueue == 0 {
		packetQueue = 1024
	}
	if packetQueue < 64 || packetQueue > 65536 {
		return TUN{}, fmt.Errorf("tun.packet_queue must be between 64 and 65536, got %d", packetQueue)
	}
	bufferPool := raw.BufferPool
	if bufferPool == 0 {
		bufferPool = 128
	}
	if bufferPool < 8 || bufferPool > 4096 {
		return TUN{}, fmt.Errorf("tun.buffer_pool must be between 8 and 4096, got %d", bufferPool)
	}
	return TUN{
		Address: address, Peer: peer, IPv6Address: ipv6Address, IPv6Peer: ipv6Peer,
		MTU: mtu, PacketQueue: packetQueue, BufferPool: bufferPool,
	}, nil
}

func compileFakeIP(raw rawFakeIP) (FakeIP, error) {
	prefix, err := netip.ParsePrefix(defaultString(raw.CIDR, "198.18.0.0/15"))
	if err != nil || !prefix.Addr().Is4() {
		return FakeIP{}, fmt.Errorf("fake_ip.cidr must be an IPv4 prefix: %q", raw.CIDR)
	}
	prefix = prefix.Masked()
	totalAddresses := uint64(1) << uint(32-prefix.Bits())
	if totalAddresses <= 11 {
		return FakeIP{}, fmt.Errorf("fake_ip.cidr %s has no usable address capacity", prefix)
	}
	dnsTTL, err := parsePositiveDuration("fake_ip.dns_ttl", defaultString(raw.DNSTTL, "1m"))
	if err != nil {
		return FakeIP{}, err
	}
	mappingTTL, err := parsePositiveDuration("fake_ip.mapping_ttl", defaultString(raw.MappingTTL, "24h"))
	if err != nil {
		return FakeIP{}, err
	}
	if mappingTTL <= dnsTTL {
		return FakeIP{}, errors.New("fake_ip.mapping_ttl must be greater than fake_ip.dns_ttl")
	}
	usableCapacity := totalAddresses - 11 // first 10 and final address are reserved
	maxMappings := raw.MaxMappings
	if maxMappings == 0 {
		maxMappings = 65536
		if uint64(maxMappings) > usableCapacity {
			maxMappings = int(usableCapacity)
		}
	}
	if maxMappings <= 0 || uint64(maxMappings) > usableCapacity {
		return FakeIP{}, fmt.Errorf("fake_ip.max_mappings must be between 1 and %d, got %d", usableCapacity, maxMappings)
	}
	persistence := defaultString(raw.PersistenceFile, "/var/lib/tun-proxy/fake-ip.yaml")
	if err := validateAbsolutePath("fake_ip.persistence_file", persistence); err != nil {
		return FakeIP{}, err
	}
	excludes := raw.Exclude
	if excludes == nil {
		excludes = []string{"localhost", "*.local", "*.lan"}
	}
	patterns := make([]DomainPattern, 0, len(excludes))
	seen := make(map[DomainPattern]struct{}, len(excludes))
	for _, value := range excludes {
		pattern, err := domainname.ParsePattern(value)
		if err != nil {
			return FakeIP{}, fmt.Errorf("fake_ip.exclude: %w", err)
		}
		if _, exists := seen[pattern]; exists {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
	}
	return FakeIP{Prefix: prefix, DNSTTL: dnsTTL, MappingTTL: mappingTTL, MaxMappings: maxMappings, PersistenceFile: persistence, Exclude: patterns}, nil
}

func compileFakeIPv6(raw rawFakeIPv6) (FakeIPv6, error) {
	if raw.CIDR == "" {
		return FakeIPv6{}, errors.New("fake_ipv6.cidr is required when fake_ipv6 is configured")
	}
	prefix, err := netip.ParsePrefix(raw.CIDR)
	if err != nil || !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
		return FakeIPv6{}, fmt.Errorf("fake_ipv6.cidr must be an IPv6 prefix: %q", raw.CIDR)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().IsPrivate() {
		return FakeIPv6{}, fmt.Errorf("fake_ipv6.cidr must use IPv6 unique-local space, got %s", prefix)
	}
	hostBits := 128 - prefix.Bits()
	var usableCapacity uint64
	if hostBits > 64 {
		usableCapacity = ^uint64(0)
	} else if hostBits == 64 {
		usableCapacity = ^uint64(0) - 10
	} else {
		totalAddresses := uint64(1) << uint(hostBits)
		if totalAddresses <= 11 {
			return FakeIPv6{}, fmt.Errorf("fake_ipv6.cidr %s has no usable address capacity", prefix)
		}
		usableCapacity = totalAddresses - 11
	}
	maxMappings := raw.MaxMappings
	if maxMappings == 0 {
		maxMappings = 65536
		if uint64(maxMappings) > usableCapacity {
			maxMappings = int(usableCapacity)
		}
	}
	if maxMappings <= 0 || uint64(maxMappings) > usableCapacity {
		return FakeIPv6{}, fmt.Errorf("fake_ipv6.max_mappings must be between 1 and %d, got %d", usableCapacity, maxMappings)
	}
	persistence := defaultString(raw.PersistenceFile, "/var/lib/tun-proxy/fake-ipv6.yaml")
	if err := validateAbsolutePath("fake_ipv6.persistence_file", persistence); err != nil {
		return FakeIPv6{}, err
	}
	return FakeIPv6{Prefix: prefix, MaxMappings: maxMappings, PersistenceFile: persistence}, nil
}

func compileDNS(raw rawDNS) (DNS, error) {
	listen, err := netip.ParseAddrPort(defaultString(raw.Listen, "127.0.0.1:53"))
	if err != nil {
		return DNS{}, fmt.Errorf("dns.listen must be an IP address and port: %w", err)
	}
	if !listen.Addr().Is4() || !listen.Addr().IsLoopback() {
		return DNS{}, fmt.Errorf("dns.listen must be loopback, got %s", listen.Addr())
	}
	if listen.Port() == 0 {
		return DNS{}, errors.New("dns.listen port must be between 1 and 65535")
	}
	udp := defaultBool(raw.UDP, true)
	tcp := defaultBool(raw.TCP, true)
	if !udp && !tcp {
		return DNS{}, errors.New("at least one of dns.udp or dns.tcp must be enabled")
	}
	if raw.DefaultOutbound == "" {
		return DNS{}, errors.New("dns.default_outbound is required")
	}
	maxConcurrent := raw.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = 256
	}
	if maxConcurrent < 1 || maxConcurrent > 65536 {
		return DNS{}, fmt.Errorf("dns.max_concurrent must be between 1 and 65536, got %d", maxConcurrent)
	}
	return DNS{Listen: listen, UDP: udp, TCP: tcp, DefaultOutbound: raw.DefaultOutbound, MaxConcurrent: maxConcurrent}, nil
}

func compileSessions(raw rawSessions) (Sessions, error) {
	maxTCPFlows := raw.MaxTCPFlows
	if maxTCPFlows == 0 {
		maxTCPFlows = 1024
	}
	if maxTCPFlows < 1 || maxTCPFlows > 1_000_000 {
		return Sessions{}, fmt.Errorf("sessions.max_tcp_flows must be between 1 and 1000000, got %d", maxTCPFlows)
	}
	idleTimeout, err := parsePositiveDuration("sessions.udp_idle_timeout", defaultString(raw.UDPIdleTimeout, "2m"))
	if err != nil {
		return Sessions{}, err
	}
	maxSessions := raw.MaxUDPSessions
	if maxSessions == 0 {
		maxSessions = 4096
	}
	if maxSessions < 1 || maxSessions > 1_000_000 {
		return Sessions{}, fmt.Errorf("sessions.max_udp_sessions must be between 1 and 1000000, got %d", maxSessions)
	}
	maxPerSource := raw.MaxUDPSessionsPerSource
	if maxPerSource == 0 {
		maxPerSource = 256
	}
	if maxPerSource < 1 || maxPerSource > maxSessions {
		return Sessions{}, fmt.Errorf("sessions.max_udp_sessions_per_source must be between 1 and sessions.max_udp_sessions (%d), got %d", maxSessions, maxPerSource)
	}
	return Sessions{
		MaxTCPFlows: maxTCPFlows, UDPIdleTimeout: idleTimeout,
		MaxUDPSessions: maxSessions, MaxUDPSessionsPerSource: maxPerSource,
	}, nil
}

func compileOutbounds(raw map[string]rawOutbound) (map[string]Outbound, error) {
	if len(raw) == 0 {
		return nil, errors.New("at least one outbound is required")
	}
	result := make(map[string]Outbound, len(raw))
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !validIdentifier(name) {
			return nil, fmt.Errorf("outbound name %q must contain only letters, digits, '_' or '-'; it cannot start with punctuation", name)
		}
		value := raw[name]
		kind := strings.ToLower(value.Type)
		if !oneOf(kind, "direct", "reject") {
			return nil, fmt.Errorf("outbound %q has unsupported type %q", name, value.Type)
		}
		outbound := Outbound{Name: name, Type: kind, Interface: value.Interface, Fallback: value.Fallback}
		if kind == "reject" {
			if value.Interface != "" || value.DNSSource != "" || len(value.DNS) != 0 || value.ConnectTimeout != "" || value.Fallback != "" {
				return nil, fmt.Errorf("reject outbound %q cannot set interface, dns_source, dns, connect_timeout or fallback", name)
			}
			result[name] = outbound
			continue
		}
		if value.Interface == "" || !validInterfaceName(value.Interface) {
			return nil, fmt.Errorf("direct outbound %q has invalid or empty interface", name)
		}
		if len(value.DNS) == 0 {
			return nil, fmt.Errorf("direct outbound %q must configure at least one DNS server", name)
		}
		outbound.DNSSource = strings.ToLower(defaultString(value.DNSSource, DNSSourceDHCP))
		if !oneOf(outbound.DNSSource, DNSSourceDHCP, DNSSourceStatic) {
			return nil, fmt.Errorf("direct outbound %q dns_source must be %q or %q, got %q", name, DNSSourceDHCP, DNSSourceStatic, value.DNSSource)
		}
		for index, server := range value.DNS {
			address, err := netip.ParseAddrPort(server)
			if err != nil {
				return nil, fmt.Errorf("outbound %q dns[%d] must be an IP address and port: %w", name, index, err)
			}
			if address.Port() == 0 || address.Addr().IsUnspecified() || address.Addr().IsLoopback() || address.Addr().Is4In6() {
				return nil, fmt.Errorf("outbound %q dns[%d] must be a non-loopback IPv4 or IPv6 address with a port, got %q", name, index, server)
			}
			outbound.DNS = append(outbound.DNS, address)
		}
		var err error
		outbound.ConnectTimeout, err = parsePositiveDuration("outbound "+name+" connect_timeout", defaultString(value.ConnectTimeout, "10s"))
		if err != nil {
			return nil, err
		}
		result[name] = outbound
	}
	return result, nil
}

func validateFallbacks(outbounds map[string]Outbound) error {
	for name, outbound := range outbounds {
		if outbound.Fallback == "" {
			continue
		}
		if _, ok := outbounds[outbound.Fallback]; !ok {
			return fmt.Errorf("outbound %q fallback %q is not defined", name, outbound.Fallback)
		}
		if outbound.Fallback == name {
			return fmt.Errorf("outbound %q cannot fall back to itself", name)
		}
	}

	const (
		unvisited = iota
		visiting
		done
	)
	states := make(map[string]int, len(outbounds))
	var visit func(string) error
	visit = func(name string) error {
		switch states[name] {
		case visiting:
			return fmt.Errorf("fallback cycle includes outbound %q", name)
		case done:
			return nil
		}
		states[name] = visiting
		if next := outbounds[name].Fallback; next != "" {
			if err := visit(next); err != nil {
				return err
			}
		}
		states[name] = done
		return nil
	}
	for name := range outbounds {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func compileRules(raw []rawRule, outbounds map[string]Outbound) ([]Rule, error) {
	if len(raw) == 0 {
		return nil, errors.New("rules must contain a final default rule")
	}
	result := make([]Rule, 0, len(raw))
	defaultCount := 0
	for index, value := range raw {
		if _, ok := outbounds[value.Outbound]; !ok {
			return nil, fmt.Errorf("rule %d references undefined outbound %q", index+1, value.Outbound)
		}
		rule := Rule{ID: index + 1, Outbound: value.Outbound}
		for _, domain := range value.Domain {
			normalized, err := domainname.Normalize(domain)
			if err != nil {
				return nil, fmt.Errorf("rule %d domain: %w", rule.ID, err)
			}
			rule.Domains = appendUnique(rule.Domains, normalized)
		}
		for _, suffix := range value.DomainSuffix {
			normalized, err := domainname.Normalize(suffix)
			if err != nil {
				return nil, fmt.Errorf("rule %d domain_suffix: %w", rule.ID, err)
			}
			rule.DomainSuffixes = appendUnique(rule.DomainSuffixes, normalized)
		}
		for _, rawPrefix := range value.IPCIDR {
			prefix, err := netip.ParsePrefix(rawPrefix)
			if err != nil || prefix.Addr().Is4In6() {
				return nil, fmt.Errorf("rule %d ip_cidr contains invalid prefix %q", rule.ID, rawPrefix)
			}
			if prefix != prefix.Masked() {
				return nil, fmt.Errorf("rule %d ip_cidr prefix %q is not canonical; use %s", rule.ID, rawPrefix, prefix.Masked())
			}
			rule.DestinationCIDRs = appendUniquePrefix(rule.DestinationCIDRs, prefix)
		}
		if rule.Default() {
			defaultCount++
			if index != len(raw)-1 {
				return nil, fmt.Errorf("default rule %d must be last", rule.ID)
			}
		}
		result = append(result, rule)
	}
	if defaultCount != 1 {
		return nil, fmt.Errorf("rules must contain exactly one default rule, got %d", defaultCount)
	}
	return result, nil
}

func appendUniquePrefix(values []netip.Prefix, value netip.Prefix) []netip.Prefix {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func parseIPv4(name, value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("%s must be a usable IPv4 address, got %q", name, value)
	}
	return address, nil
}

func parseIPv6(name, value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is6() || address.Is4In6() || address.IsUnspecified() || address.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("%s must be a unicast IPv6 address: %q", name, value)
	}
	return address, nil
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", name, value)
	}
	return duration, nil
}

func validateAbsolutePath(name, value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path, got %q", name, value)
	}
	return nil
}

func validIdentifier(value string) bool {
	if value == "" || value[0] == '-' || value[0] == '_' {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func validInterfaceName(value string) bool {
	if value == "" || len(value) > 15 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func defaultBool(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func appendUniquePort(values []uint16, value uint16) []uint16 {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
