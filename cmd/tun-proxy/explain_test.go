package main

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestExplainFlowReportsConclusiveDomainRule(t *testing.T) {
	runtime, err := config.LoadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result, err := explainFlow(context.Background(), runtime, "config.yaml", "api.cursor.sh", nil, "tcp", 443, "ipv4", false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.PendingDNS || result.FinalDecision == nil {
		t.Fatalf("unexpected pending result: %+v", result)
	}
	if result.FinalDecision.RuleID != 1 || result.FinalDecision.Outbound.Name != "wifi" || result.FinalDecision.Outbound.Interface != "en0" || result.FinalDecision.Outbound.DNSSource != config.DNSSourceDHCP {
		t.Fatalf("final decision = %+v", result.FinalDecision)
	}
}

func TestExplainFlowResolvesDeferredCIDRWithProvidedIP(t *testing.T) {
	runtime := &config.Config{
		DNS: config.DNS{MaxConcurrent: 4},
		Outbounds: map[string]config.Outbound{
			"special": {Name: "special", Type: "direct", Interface: "en7"},
			"default": {Name: "default", Type: "direct", Interface: "en0"},
		},
		Rules: []config.Rule{
			{ID: 1, DestinationCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Outbound: "special"},
			{ID: 2, Outbound: "default"},
		},
	}
	pending, err := explainFlow(context.Background(), runtime, "config.yaml", "example.com", nil, "tcp", 443, "ipv4", false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !pending.PendingDNS || pending.FinalDecision != nil || len(pending.Candidates) != 2 {
		t.Fatalf("pending explanation = %+v", pending)
	}
	resolved, err := explainFlow(context.Background(), runtime, "config.yaml", "example.com", []netip.Addr{netip.MustParseAddr("203.0.113.9")}, "tcp", 443, "ipv4", false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.FinalDecision == nil || resolved.FinalDecision.RuleID != 1 || resolved.FinalDecision.Outbound.Name != "special" {
		t.Fatalf("resolved explanation = %+v", resolved)
	}
}

func TestExplainSuffixRespectsLabelBoundary(t *testing.T) {
	runtime, err := config.LoadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result, err := explainFlow(context.Background(), runtime, "config.yaml", "not-cursor.sh", nil, "tcp", 443, "ipv4", false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalDecision == nil || result.FinalDecision.Outbound.Name != "wired" || result.FinalDecision.Outbound.Interface != "en7" {
		t.Fatalf("not-cursor.sh incorrectly matched cursor.sh suffix: %+v", result.FinalDecision)
	}
}
