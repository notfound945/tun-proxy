//go:build darwin

package system

import (
	"net/netip"
	"strings"
	"testing"
)

func TestConfigureTUNUsesFixedIfconfigArguments(t *testing.T) {
	runner := &fakeRunner{responses: make(map[string]string), fail: make(map[string]error)}
	err := ConfigureTUN(t.Context(), runner, "utun7", netip.MustParseAddr("10.255.0.2"), netip.MustParseAddr("10.255.0.1"), 1400)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].executable != "/sbin/ifconfig" {
		t.Fatalf("calls = %#v", runner.calls)
	}
	want := "utun7 inet 10.255.0.2 10.255.0.1 netmask 255.255.255.255 mtu 1400 up"
	if got := strings.Join(runner.calls[0].args, " "); got != want {
		t.Fatalf("ifconfig args = %q, want %q", got, want)
	}
}

func TestConfigureTUNRejectsUntrustedArguments(t *testing.T) {
	runner := &fakeRunner{responses: make(map[string]string), fail: make(map[string]error)}
	err := ConfigureTUN(t.Context(), runner, "utun7; reboot", netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1"), 1400)
	if err == nil || !strings.Contains(err.Error(), "invalid utun") {
		t.Fatalf("ConfigureTUN() = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid input invoked runner: %#v", runner.calls)
	}
}

func TestConfigureTUNIPv6UsesFixedIfconfigArguments(t *testing.T) {
	runner := &fakeRunner{responses: make(map[string]string), fail: make(map[string]error)}
	err := ConfigureTUNIPv6(t.Context(), runner, "utun7", netip.MustParseAddr("fd00:ffff::2"), netip.MustParseAddr("fd00:ffff::1"))
	if err != nil {
		t.Fatal(err)
	}
	want := "utun7 inet6 fd00:ffff::2 fd00:ffff::1 prefixlen 128"
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].args, " ") != want {
		t.Fatalf("calls = %#v, want args %q", runner.calls, want)
	}
}

func TestConfigureTUNIPv6RejectsWrongFamily(t *testing.T) {
	runner := &fakeRunner{responses: make(map[string]string), fail: make(map[string]error)}
	err := ConfigureTUNIPv6(t.Context(), runner, "utun7", netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("fd00:ffff::1"))
	if err == nil || !strings.Contains(err.Error(), "invalid IPv6") {
		t.Fatalf("ConfigureTUNIPv6() = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid input invoked runner: %#v", runner.calls)
	}
}
