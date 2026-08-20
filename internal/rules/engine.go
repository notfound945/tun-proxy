// Package rules implements deterministic, side-effect-free flow matching.
package rules

import (
	"errors"
	"net/netip"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/domainname"
)

// FlowMetadata is the complete immutable input to one rules decision.
type FlowMetadata struct {
	Domain          string
	FakeIP          netip.Addr
	DestinationIP   netip.Addr
	SourceIP        netip.Addr
	SourcePort      uint16
	DestinationPort uint16
	Protocol        string
}

type Decision struct {
	RuleID   int
	Outbound string
}

type compiledRule struct {
	id       int
	domains  map[string]struct{}
	suffixes []string
	cidrs    []netip.Prefix
	protocol string
	ports    map[uint16]struct{}
	outbound string
}

// Engine is immutable after construction and safe for concurrent matching.
type Engine struct {
	rules []compiledRule
}

func New(configRules []config.Rule) (*Engine, error) {
	if len(configRules) == 0 || !configRules[len(configRules)-1].Default() {
		return nil, errors.New("rules engine requires a final default rule")
	}
	compiled := make([]compiledRule, 0, len(configRules))
	for _, rule := range configRules {
		item := compiledRule{
			id:       rule.ID,
			protocol: rule.Protocol,
			outbound: rule.Outbound,
			domains:  make(map[string]struct{}, len(rule.Domains)),
			ports:    make(map[uint16]struct{}, len(rule.DestinationPorts)),
		}
		for _, domain := range rule.Domains {
			item.domains[domain] = struct{}{}
		}
		item.suffixes = append([]string(nil), rule.DomainSuffixes...)
		item.cidrs = append([]netip.Prefix(nil), rule.DestinationCIDRs...)
		for _, port := range rule.DestinationPorts {
			item.ports[port] = struct{}{}
		}
		compiled = append(compiled, item)
	}
	return &Engine{rules: compiled}, nil
}

func (engine *Engine) Match(metadata FlowMetadata) (Decision, error) {
	candidates, err := engine.Candidates(metadata)
	if err != nil {
		return Decision{}, err
	}
	return candidates[len(candidates)-1], nil
}

// Candidates returns CIDR-deferred rules whose non-IP predicates match,
// followed by the first conclusive rule without CIDRs. The final default rule
// guarantees at least one result. Session policy normally resolves through
// the conclusive outbound, but can use an earlier resolvable candidate when
// that outbound is reject-only.
func (engine *Engine) Candidates(metadata FlowMetadata) ([]Decision, error) {
	domain, protocol, err := normalizeMetadata(metadata)
	if err != nil {
		return nil, err
	}
	var candidates []Decision
	for _, rule := range engine.rules {
		if !matchesBase(rule, domain, protocol, metadata.DestinationPort) {
			continue
		}
		candidates = append(candidates, Decision{RuleID: rule.id, Outbound: rule.outbound})
		if len(rule.cidrs) == 0 {
			return candidates, nil
		}
	}
	return nil, errors.New("no candidate matched; final default rule invariant was violated")
}

// MatchResolved performs the final ordered match after the candidate
// outbound has resolved the domain. A CIDR predicate matches when at least one
// candidate address is contained by at least one configured prefix.
func (engine *Engine) MatchResolved(metadata FlowMetadata, addresses []netip.Addr) (Decision, error) {
	for _, address := range addresses {
		if !address.IsValid() || address.Is4In6() {
			return Decision{}, errors.New("resolved addresses contain an invalid IP")
		}
	}
	return engine.match(metadata, addresses, true)
}

func (engine *Engine) match(metadata FlowMetadata, addresses []netip.Addr, resolved bool) (Decision, error) {
	domain, protocol, err := normalizeMetadata(metadata)
	if err != nil {
		return Decision{}, err
	}
	for _, rule := range engine.rules {
		if len(rule.cidrs) != 0 {
			if !resolved || !matchesAnyCIDR(addresses, rule.cidrs) {
				continue
			}
		}
		if !matchesBase(rule, domain, protocol, metadata.DestinationPort) {
			continue
		}
		return Decision{RuleID: rule.id, Outbound: rule.outbound}, nil
	}
	return Decision{}, errors.New("no rule matched; final default rule invariant was violated")
}

func normalizeMetadata(metadata FlowMetadata) (string, string, error) {
	domain := ""
	if metadata.Domain != "" {
		normalized, err := domainname.Normalize(metadata.Domain)
		if err != nil {
			return "", "", err
		}
		domain = normalized
	}
	return domain, strings.ToLower(metadata.Protocol), nil
}

func matchesBase(rule compiledRule, domain, protocol string, destinationPort uint16) bool {
	if rule.protocol != "" && rule.protocol != protocol {
		return false
	}
	if len(rule.ports) != 0 {
		if _, ok := rule.ports[destinationPort]; !ok {
			return false
		}
	}
	if len(rule.domains) != 0 {
		if _, ok := rule.domains[domain]; !ok {
			return false
		}
	}
	return len(rule.suffixes) == 0 || (domain != "" && matchesAnySuffix(domain, rule.suffixes))
}

func matchesAnyCIDR(addresses []netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		for _, address := range addresses {
			if prefix.Contains(address) {
				return true
			}
		}
	}
	return false
}

func matchesAnySuffix(domain string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if domain == suffix || strings.HasSuffix(domain, "."+suffix) {
			return true
		}
	}
	return false
}
