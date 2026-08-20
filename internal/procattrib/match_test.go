package procattrib

import (
	"net/netip"
	"testing"
)

func TestMatches(t *testing.T) {
	t.Parallel()
	tcp := Flow{
		Protocol:    TCP,
		Source:      netip.MustParseAddrPort("10.255.0.2:49152"),
		Destination: netip.MustParseAddrPort("198.18.0.37:443"),
	}
	tests := []struct {
		name   string
		flow   Flow
		socket socketRecord
		want   bool
	}{
		{
			name: "exact tcp",
			flow: tcp,
			socket: socketRecord{protocol: TCP, local: tcp.Source,
				remote: tcp.Destination},
			want: true,
		},
		{
			name: "wrong destination",
			flow: tcp,
			socket: socketRecord{protocol: TCP, local: tcp.Source,
				remote: netip.MustParseAddrPort("198.18.0.38:443")},
		},
		{
			name: "wildcard local address",
			flow: tcp,
			socket: socketRecord{protocol: TCP,
				local: netip.MustParseAddrPort("0.0.0.0:49152"), remote: tcp.Destination},
			want: true,
		},
		{
			name: "unconnected udp",
			flow: Flow{Protocol: UDP, Source: tcp.Source, Destination: tcp.Destination},
			socket: socketRecord{protocol: UDP, local: tcp.Source,
				remote: netip.MustParseAddrPort("0.0.0.0:0"), remoteOpen: true},
			want: true,
		},
		{
			name: "unconnected tcp invalid",
			flow: tcp,
			socket: socketRecord{protocol: TCP, local: tcp.Source,
				remote: netip.MustParseAddrPort("0.0.0.0:0"), remoteOpen: true},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := matches(test.flow, test.socket); got != test.want {
				t.Fatalf("matches() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestOutcomeForOwners(t *testing.T) {
	t.Parallel()
	if got := outcomeForOwners(nil); got != OutcomeNone {
		t.Fatalf("empty outcome = %q", got)
	}
	if got := outcomeForOwners([]Owner{{PID: 1}}); got != OutcomeUnique {
		t.Fatalf("unique outcome = %q", got)
	}
	if got := outcomeForOwners([]Owner{{PID: 1}, {PID: 2}}); got != OutcomeAmbiguous {
		t.Fatalf("ambiguous outcome = %q", got)
	}
}

func TestFlowValidate(t *testing.T) {
	t.Parallel()
	valid := Flow{
		Protocol:    TCP,
		Source:      netip.MustParseAddrPort("127.0.0.1:1234"),
		Destination: netip.MustParseAddrPort("127.0.0.1:443"),
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Destination = netip.MustParseAddrPort("[::1]:443")
	if err := invalid.Validate(); err == nil {
		t.Fatal("mixed address families accepted")
	}
	invalid = valid
	invalid.Source = netip.MustParseAddrPort("127.0.0.1:0")
	if err := invalid.Validate(); err == nil {
		t.Fatal("zero source port accepted")
	}
	mapped := valid
	mapped.Source = netip.MustParseAddrPort("[::ffff:127.0.0.1]:1234")
	if err := mapped.Validate(); err != nil {
		t.Fatalf("IPv4-mapped source rejected: %v", err)
	}
}
