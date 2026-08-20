package rules

import (
	"net/netip"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestEngineFirstMatchingRuleWins(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, Domains: []string{"api.example.com"}, Protocol: "tcp", DestinationPorts: []uint16{443}, Outbound: "wired"},
		{ID: 2, DomainSuffixes: []string{"example.com"}, Protocol: "tcp", Outbound: "wifi"},
		{ID: 3, Outbound: "reject"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		metadata FlowMetadata
		want     Decision
	}{
		{
			name:     "exact combined match",
			metadata: FlowMetadata{Domain: "API.Example.COM.", Protocol: "TCP", DestinationPort: 443},
			want:     Decision{RuleID: 1, Outbound: "wired"},
		},
		{
			name:     "suffix includes subdomain",
			metadata: FlowMetadata{Domain: "cdn.example.com", Protocol: "tcp", DestinationPort: 80},
			want:     Decision{RuleID: 2, Outbound: "wifi"},
		},
		{
			name:     "suffix respects label boundary",
			metadata: FlowMetadata{Domain: "notexample.com", Protocol: "tcp", DestinationPort: 80},
			want:     Decision{RuleID: 3, Outbound: "reject"},
		},
		{
			name:     "all constraints must match",
			metadata: FlowMetadata{Domain: "api.example.com", Protocol: "udp", DestinationPort: 443},
			want:     Decision{RuleID: 3, Outbound: "reject"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := engine.Match(test.metadata)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Match() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestEngineCarriesNoNetworkTypes(t *testing.T) {
	engine, err := New([]config.Rule{{ID: 1, Outbound: "direct"}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.Match(FlowMetadata{
		Domain: "example.com", FakeIP: netip.MustParseAddr("198.18.0.10"),
		SourceIP: netip.MustParseAddr("10.0.0.2"), SourcePort: 1234,
		DestinationPort: 443, Protocol: "tcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outbound != "direct" {
		t.Fatalf("outbound = %q", decision.Outbound)
	}
}

func TestEngineRequiresDefaultRule(t *testing.T) {
	_, err := New([]config.Rule{{ID: 1, Domains: []string{"example.com"}, Outbound: "direct"}})
	if err == nil {
		t.Fatal("New returned nil error without a default rule")
	}
}

func TestEngineRoutesLiteralWithoutMatchingDomainRules(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, Domains: []string{"example.com"}, Outbound: "domain"},
		{ID: 2, Protocol: "tcp", DestinationPorts: []uint16{443}, Outbound: "literal-https"},
		{ID: 3, Outbound: "default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.Match(FlowMetadata{
		DestinationIP: netip.MustParseAddr("203.0.113.9"), Protocol: "tcp", DestinationPort: 443,
	})
	if err != nil || decision.RuleID != 2 || decision.Outbound != "literal-https" {
		t.Fatalf("literal decision = %+v, %v", decision, err)
	}
}

func TestEngineDefersCIDRRulesUntilResolved(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "special"},
		{ID: 2, DomainSuffixes: []string{"example.com"}, Outbound: "candidate"},
		{ID: 3, Outbound: "default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := FlowMetadata{Domain: "api.example.com", Protocol: "tcp", DestinationPort: 443}
	candidate, err := engine.Match(metadata)
	if err != nil || candidate != (Decision{RuleID: 2, Outbound: "candidate"}) {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	candidates, err := engine.Candidates(metadata)
	wantCandidates := []Decision{{RuleID: 1, Outbound: "special"}, {RuleID: 2, Outbound: "candidate"}}
	if err != nil || len(candidates) != len(wantCandidates) || candidates[0] != wantCandidates[0] || candidates[1] != wantCandidates[1] {
		t.Fatalf("candidates = %+v, %v", candidates, err)
	}
	final, err := engine.MatchResolved(metadata, []netip.Addr{
		netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("203.0.113.10"),
	})
	if err != nil || final != (Decision{RuleID: 1, Outbound: "special"}) {
		t.Fatalf("final = %+v, %v", final, err)
	}
}

func TestEngineCIDRRulePreservesOrderAndAllConstraints(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, Domains: []string{"other.example"}, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "wrong-domain"},
		{ID: 2, Protocol: "udp", DestinationPorts: []uint16{53}, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "dns"},
		{ID: 3, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}, Outbound: "v6"},
		{ID: 4, Outbound: "default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := FlowMetadata{Domain: "example.com", Protocol: "udp", DestinationPort: 53}
	decision, err := engine.MatchResolved(metadata, []netip.Addr{netip.MustParseAddr("203.0.113.53")})
	if err != nil || decision != (Decision{RuleID: 2, Outbound: "dns"}) {
		t.Fatalf("IPv4 decision = %+v, %v", decision, err)
	}
	decision, err = engine.MatchResolved(FlowMetadata{Protocol: "tcp"}, []netip.Addr{netip.MustParseAddr("2001:db8::10")})
	if err != nil || decision != (Decision{RuleID: 3, Outbound: "v6"}) {
		t.Fatalf("IPv6 decision = %+v, %v", decision, err)
	}
}
