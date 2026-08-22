package rules

import (
	"net/netip"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestEngineFirstMatchingRuleWins(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, Domains: []string{"api.example.com"}, DomainSuffixes: []string{"example.com"}, Outbound: "exact"},
		{ID: 2, DomainSuffixes: []string{"example.com"}, Outbound: "suffix"},
		{ID: 3, Outbound: "default"},
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
			name:     "exact domain and suffix both match",
			metadata: FlowMetadata{Domain: "API.Example.COM."},
			want:     Decision{RuleID: 1, Outbound: "exact"},
		},
		{
			name:     "suffix includes subdomain",
			metadata: FlowMetadata{Domain: "cdn.example.com"},
			want:     Decision{RuleID: 2, Outbound: "suffix"},
		},
		{
			name:     "suffix respects label boundary",
			metadata: FlowMetadata{Domain: "notexample.com"},
			want:     Decision{RuleID: 3, Outbound: "default"},
		},
		{
			name:     "different fields are combined with AND",
			metadata: FlowMetadata{Domain: "other.example.com"},
			want:     Decision{RuleID: 2, Outbound: "suffix"},
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

func TestEngineIgnoresNonRuleFlowMetadata(t *testing.T) {
	engine, err := New([]config.Rule{{ID: 1, Outbound: "direct"}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.Match(FlowMetadata{
		Domain: "example.com", FakeIP: netip.MustParseAddr("198.18.0.10"),
		DestinationIP: netip.MustParseAddr("203.0.113.10"),
		SourceIP:      netip.MustParseAddr("10.0.0.2"), SourcePort: 1234,
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

func TestEngineRoutesLiteralThroughDefaultWithoutDomainOrCIDRMatch(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, Domains: []string{"example.com"}, Outbound: "domain"},
		{ID: 2, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, Outbound: "cidr"},
		{ID: 3, Outbound: "default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := FlowMetadata{DestinationIP: netip.MustParseAddr("203.0.113.9")}
	decision, err := engine.MatchResolved(metadata, []netip.Addr{metadata.DestinationIP})
	if err != nil || decision != (Decision{RuleID: 3, Outbound: "default"}) {
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
	metadata := FlowMetadata{Domain: "api.example.com"}
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

func TestEngineCIDRRulesPreserveOrderAndDomainConstraints(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, Domains: []string{"other.example"}, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "wrong-domain"},
		{ID: 2, DomainSuffixes: []string{"example.com"}, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "domain-cidr"},
		{ID: 3, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}, Outbound: "v6"},
		{ID: 4, Outbound: "default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.MatchResolved(FlowMetadata{Domain: "api.example.com"}, []netip.Addr{netip.MustParseAddr("203.0.113.53")})
	if err != nil || decision != (Decision{RuleID: 2, Outbound: "domain-cidr"}) {
		t.Fatalf("IPv4 decision = %+v, %v", decision, err)
	}
	decision, err = engine.MatchResolved(FlowMetadata{}, []netip.Addr{netip.MustParseAddr("2001:db8::10")})
	if err != nil || decision != (Decision{RuleID: 3, Outbound: "v6"}) {
		t.Fatalf("IPv6 decision = %+v, %v", decision, err)
	}
}

func TestEngineRequiresFakeIPForDomainPredicates(t *testing.T) {
	engine, err := New([]config.Rule{
		{ID: 1, Domains: []string{"api.example.com"}, Outbound: "exact"},
		{ID: 2, DomainSuffixes: []string{"video.example"}, Outbound: "suffix"},
		{ID: 3, Domains: []string{"both.example.com"}, DomainSuffixes: []string{"example.com"}, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "combined"},
		{ID: 4, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}, Outbound: "cidr-only"},
		{ID: 5, Outbound: "default"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		domain string
		want   bool
	}{
		{domain: "API.Example.COM.", want: true},
		{domain: "video.example", want: true},
		{domain: "cdn.video.example", want: true},
		{domain: "notvideo.example", want: false},
		{domain: "both.example.com", want: true},
		{domain: "ordinary.example", want: false},
		{domain: "invalid..example", want: false},
	} {
		t.Run(test.domain, func(t *testing.T) {
			if got := engine.RequiresFakeIP(test.domain); got != test.want {
				t.Fatalf("RequiresFakeIP(%q) = %t, want %t", test.domain, got, test.want)
			}
		})
	}
}
