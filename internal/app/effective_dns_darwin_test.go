//go:build darwin

package app

import (
	"context"
	"net/netip"
	"reflect"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestRuntimeWithInterfaceDNSPrefersDHCPAndKeepsConfiguredFallback(t *testing.T) {
	runtime := &config.Config{Outbounds: map[string]config.Outbound{
		"wifi": {
			Name: "wifi", Type: "direct", Interface: "en0",
			DNSSource: config.DNSSourceDHCP,
			DNS:       []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:53")},
		},
		"wired": {
			Name: "wired", Type: "direct", Interface: "en7",
			DNSSource: config.DNSSourceStatic,
			DNS:       []netip.AddrPort{netip.MustParseAddrPort("9.9.9.9:53")},
		},
		"backup": {
			Name: "backup", Type: "direct", Interface: "en8",
			DNSSource: config.DNSSourceDHCP,
			DNS:       []netip.AddrPort{netip.MustParseAddrPort("8.8.8.8:53")},
		},
		"reject": {Name: "reject", Type: "reject"},
	}}
	discovered := interfaceDNS{
		"en0": {netip.MustParseAddrPort("192.168.1.1:53")},
		"en7": {netip.MustParseAddrPort("192.168.7.1:53")},
	}
	effective := runtimeWithInterfaceDNS(runtime, discovered)
	if got, want := effective.Outbounds["wifi"].DNS, discovered["en0"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("wifi DNS = %v, want DHCP %v", got, want)
	}
	if got, want := effective.Outbounds["wired"].DNS, runtime.Outbounds["wired"].DNS; !reflect.DeepEqual(got, want) {
		t.Fatalf("wired DNS = %v, want static DNS %v", got, want)
	}
	if got, want := effective.Outbounds["backup"].DNS, runtime.Outbounds["backup"].DNS; !reflect.DeepEqual(got, want) {
		t.Fatalf("backup DNS = %v, want configured fallback %v", got, want)
	}
	if got := runtime.Outbounds["wifi"].DNS; !reflect.DeepEqual(got, []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:53")}) {
		t.Fatalf("source runtime was mutated: %v", got)
	}
}

type effectiveDNSTestRunner struct {
	calls []string
}

func (runner *effectiveDNSTestRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, args[len(args)-1])
	return []byte("domain_name_server (ip_mult): {192.168.1.1}\n"), nil
}

func TestDiscoverInterfaceDNSSkipsStaticOutbounds(t *testing.T) {
	runtime := &config.Config{Outbounds: map[string]config.Outbound{
		"dhcp":   {Name: "dhcp", Type: "direct", Interface: "en0", DNSSource: config.DNSSourceDHCP},
		"static": {Name: "static", Type: "direct", Interface: "en7", DNSSource: config.DNSSourceStatic},
	}}
	runner := &effectiveDNSTestRunner{}
	discovered, err := discoverInterfaceDNS(t.Context(), runtime, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.calls, []string{"en0"}) {
		t.Fatalf("DHCP discovery interfaces = %v, want [en0]", runner.calls)
	}
	if _, exists := discovered["en7"]; exists {
		t.Fatalf("static interface unexpectedly discovered: %v", discovered)
	}
}

func TestEffectiveDNSMessagesDescribeSelectedSource(t *testing.T) {
	runtime := &config.Config{Outbounds: map[string]config.Outbound{
		"dhcp":     {Name: "dhcp", Type: "direct", Interface: "en0", DNSSource: config.DNSSourceDHCP, DNS: []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:53")}},
		"fallback": {Name: "fallback", Type: "direct", Interface: "en7", DNSSource: config.DNSSourceDHCP, DNS: []netip.AddrPort{netip.MustParseAddrPort("9.9.9.9:53")}},
		"static":   {Name: "static", Type: "direct", Interface: "en8", DNSSource: config.DNSSourceStatic, DNS: []netip.AddrPort{netip.MustParseAddrPort("8.8.8.8:53")}},
	}}
	messages := effectiveDNSMessages(runtime, interfaceDNS{"en0": {netip.MustParseAddrPort("192.168.1.1:53")}})
	want := []string{
		"effective DNS outbound=dhcp interface=en0 source=dhcp servers=192.168.1.1:53",
		"effective DNS outbound=fallback interface=en7 source=configured-fallback servers=9.9.9.9:53",
		"effective DNS outbound=static interface=en8 source=static servers=8.8.8.8:53",
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("effective DNS messages = %#v, want %#v", messages, want)
	}
}
