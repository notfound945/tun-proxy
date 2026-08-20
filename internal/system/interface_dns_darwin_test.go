//go:build darwin

package system

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

func TestDiscoverInterfaceDNSUsesDHCPOrderAndFallsBackOnMissingPacket(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		ipconfig + " getpacket en0": `options:
domain_name_server (ip_mult): {8.8.8.8, 8.8.4.4, 8.8.8.8}
`,
	}, fail: map[string]error{
		ipconfig + " getpacket en7": errors.New("no DHCP packet"),
	}}
	got, err := DiscoverInterfaceDNS(t.Context(), runner, []string{"en7", "en0", "en0"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]netip.AddrPort{
		"en0": {netip.MustParseAddrPort("8.8.8.8:53"), netip.MustParseAddrPort("8.8.4.4:53")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverInterfaceDNS() = %#v, want %#v", got, want)
	}
}

func TestParseDHCPDNSFiltersUnsafeAddresses(t *testing.T) {
	got, err := parseDHCPDNS(`
domain_name_server (ip_mult): {127.0.0.1, 0.0.0.0, 224.0.0.1, ::ffff:1.1.1.1, 9.9.9.9}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("1.1.1.1:53"),
		netip.MustParseAddrPort("9.9.9.9:53"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDHCPDNS() = %#v, want %#v", got, want)
	}
}

func TestParseDHCPDNSRejectsMalformedAddress(t *testing.T) {
	if _, err := parseDHCPDNS("domain_name_server (ip_mult): {not-an-ip}\n"); err == nil {
		t.Fatal("parseDHCPDNS accepted a malformed DNS address")
	}
}
