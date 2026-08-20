package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/hailinpan/tun-proxy/internal/resolver"
	"github.com/hailinpan/tun-proxy/internal/rules"
)

type addressList []netip.Addr

func (values *addressList) String() string {
	items := make([]string, 0, len(*values))
	for _, value := range *values {
		items = append(items, value.String())
	}
	return strings.Join(items, ",")
}

func (values *addressList) Set(raw string) error {
	address, err := netip.ParseAddr(raw)
	if err != nil || address.Is4In6() {
		return fmt.Errorf("invalid IP address %q", raw)
	}
	*values = append(*values, address)
	return nil
}

type explainInput struct {
	Domain   string   `json:"domain,omitempty"`
	IPs      []string `json:"ips,omitempty"`
	Protocol string   `json:"protocol"`
	Port     uint16   `json:"port"`
	Family   string   `json:"family,omitempty"`
}

type explainRule struct {
	ID               int      `json:"id"`
	Domains          []string `json:"domains,omitempty"`
	DomainSuffixes   []string `json:"domain_suffixes,omitempty"`
	DestinationCIDRs []string `json:"destination_cidrs,omitempty"`
	Protocol         string   `json:"protocol,omitempty"`
	DestinationPorts []uint16 `json:"destination_ports,omitempty"`
	Outbound         string   `json:"outbound"`
	DeferredUntilDNS bool     `json:"deferred_until_dns"`
}

type explainOutbound struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Interface      string   `json:"interface,omitempty"`
	DNSSource      string   `json:"dns_source,omitempty"`
	DNS            []string `json:"dns,omitempty"`
	ConnectTimeout string   `json:"connect_timeout,omitempty"`
	Fallback       string   `json:"fallback,omitempty"`
}

type explainDecision struct {
	RuleID   int             `json:"rule_id"`
	Outbound explainOutbound `json:"outbound"`
}

type explainResult struct {
	Config             string           `json:"config"`
	Input              explainInput     `json:"input"`
	Candidates         []explainRule    `json:"candidates"`
	CandidateDecision  explainDecision  `json:"candidate_decision"`
	ResolutionOutbound string           `json:"resolution_outbound,omitempty"`
	ResolvedAddresses  []string         `json:"resolved_addresses,omitempty"`
	FinalDecision      *explainDecision `json:"final_decision,omitempty"`
	PendingDNS         bool             `json:"pending_dns"`
	Note               string           `json:"note,omitempty"`
}

type explainOptions struct {
	configPath string
	domain     string
	protocol   string
	port       int
	family     string
	resolve    bool
	timeout    time.Duration
	jsonOutput bool
	addresses  addressList
}

func newExplainFlagSet(output io.Writer, options *explainOptions) *flag.FlagSet {
	if options == nil {
		options = &explainOptions{}
	}
	flags := newCommandFlagSet("explain", output)
	flags.StringVar(&options.configPath, "config", defaultUserConfigPath(), "YAML configuration `PATH`")
	flags.StringVar(&options.domain, "domain", "", "destination `NAME`")
	flags.StringVar(&options.protocol, "protocol", "tcp", "flow `PROTOCOL` (tcp or udp)")
	flags.IntVar(&options.port, "port", 443, "destination `PORT`")
	flags.StringVar(&options.family, "family", "ipv4", "resolution `FAMILY` (ipv4 or ipv6)")
	flags.BoolVar(&options.resolve, "resolve", false, "resolve through configured interface-bound DNS")
	flags.DurationVar(&options.timeout, "timeout", 10*time.Second, "DNS resolution `DURATION`")
	flags.BoolVar(&options.jsonOutput, "json", false, "print explanation as JSON")
	flags.Var(&options.addresses, "ip", "resolved or literal `ADDRESS` (repeatable)")
	return flags
}

func explainCommand(args []string) error {
	options := explainOptions{}
	flags := newExplainFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("explain received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if options.domain == "" && len(options.addresses) == 0 {
		return errors.New("explain requires -domain or at least one -ip")
	}
	options.protocol = strings.ToLower(options.protocol)
	if options.protocol != "tcp" && options.protocol != "udp" {
		return fmt.Errorf("protocol must be tcp or udp, got %q", options.protocol)
	}
	if options.port < 1 || options.port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", options.port)
	}
	options.family = strings.ToLower(options.family)
	if options.family != "ipv4" && options.family != "ipv6" {
		return fmt.Errorf("family must be ipv4 or ipv6, got %q", options.family)
	}
	if options.resolve && len(options.addresses) != 0 {
		return errors.New("-resolve and -ip cannot be used together")
	}
	if options.resolve && options.domain == "" {
		return errors.New("-resolve requires -domain")
	}
	if options.timeout <= 0 {
		return errors.New("timeout must be positive")
	}

	runtime, err := config.LoadFile(options.configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	result, err := explainFlow(ctx, runtime, options.configPath, options.domain, options.addresses, options.protocol, uint16(options.port), options.family, options.resolve, options.timeout)
	if err != nil {
		return err
	}
	if options.jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	printExplainResult(result)
	return nil
}

func explainFlow(
	ctx context.Context,
	runtime *config.Config,
	configPath string,
	domain string,
	addresses []netip.Addr,
	protocol string,
	port uint16,
	family string,
	resolve bool,
	resolveTimeout time.Duration,
) (explainResult, error) {
	engine, err := rules.New(runtime.Rules)
	if err != nil {
		return explainResult{}, err
	}
	metadata := rules.FlowMetadata{Domain: domain, Protocol: protocol, DestinationPort: port}
	candidates, err := engine.Candidates(metadata)
	if err != nil {
		return explainResult{}, err
	}
	result := explainResult{
		Config:     configPath,
		Input:      explainInput{Domain: domain, Protocol: protocol, Port: port, Family: family},
		Candidates: make([]explainRule, 0, len(candidates)),
	}
	for _, address := range addresses {
		result.Input.IPs = append(result.Input.IPs, address.String())
	}
	for index, candidate := range candidates {
		rule, ok := findRule(runtime.Rules, candidate.RuleID)
		if !ok {
			return explainResult{}, fmt.Errorf("candidate rule %d is missing from configuration", candidate.RuleID)
		}
		result.Candidates = append(result.Candidates, summarizeRule(rule, index < len(candidates)-1))
	}
	conclusive := candidates[len(candidates)-1]
	result.CandidateDecision, err = summarizeDecision(runtime, conclusive)
	if err != nil {
		return explainResult{}, err
	}

	if len(addresses) == 0 && resolve {
		resolutionDecision, needsDNS, err := resolutionCandidate(runtime, candidates)
		if err != nil {
			return explainResult{}, err
		}
		if !needsDNS {
			final := result.CandidateDecision
			result.FinalDecision = &final
			result.Note = "all matching candidates reject without DNS resolution"
			return result, nil
		}
		addresses, result.ResolutionOutbound, err = resolveConfigured(ctx, runtime, resolutionDecision.Outbound, domain, family, resolveTimeout)
		if err != nil {
			return explainResult{}, err
		}
		for _, address := range addresses {
			result.ResolvedAddresses = append(result.ResolvedAddresses, address.String())
		}
	}

	if len(addresses) != 0 {
		final, err := engine.MatchResolved(metadata, addresses)
		if err != nil {
			return explainResult{}, err
		}
		summary, err := summarizeDecision(runtime, final)
		if err != nil {
			return explainResult{}, err
		}
		result.FinalDecision = &summary
		if result.ResolutionOutbound != "" && final.Outbound != result.ResolutionOutbound {
			result.Note = "the final rule changes outbound; production keeps this decision immutable and re-resolves through the selected outbound"
		}
		return result, nil
	}

	if len(candidates) > 1 {
		result.PendingDNS = true
		result.Note = "one or more earlier CIDR rules can still win after DNS resolution"
		return result, nil
	}
	final := result.CandidateDecision
	result.FinalDecision = &final
	return result, nil
}

func findRule(configRules []config.Rule, id int) (config.Rule, bool) {
	for _, rule := range configRules {
		if rule.ID == id {
			return rule, true
		}
	}
	return config.Rule{}, false
}

func summarizeRule(rule config.Rule, deferred bool) explainRule {
	result := explainRule{
		ID: rule.ID, Domains: append([]string(nil), rule.Domains...),
		DomainSuffixes: append([]string(nil), rule.DomainSuffixes...),
		Protocol:       rule.Protocol, DestinationPorts: append([]uint16(nil), rule.DestinationPorts...),
		Outbound: rule.Outbound, DeferredUntilDNS: deferred,
	}
	for _, prefix := range rule.DestinationCIDRs {
		result.DestinationCIDRs = append(result.DestinationCIDRs, prefix.String())
	}
	return result
}

func summarizeDecision(runtime *config.Config, decision rules.Decision) (explainDecision, error) {
	configured, ok := runtime.Outbounds[decision.Outbound]
	if !ok {
		return explainDecision{}, fmt.Errorf("outbound %q is not configured", decision.Outbound)
	}
	result := explainDecision{RuleID: decision.RuleID, Outbound: explainOutbound{
		Name: configured.Name, Type: configured.Type, Interface: configured.Interface,
		DNSSource: configured.DNSSource, ConnectTimeout: configured.ConnectTimeout.String(), Fallback: configured.Fallback,
	}}
	for _, server := range configured.DNS {
		result.Outbound.DNS = append(result.Outbound.DNS, server.String())
	}
	return result, nil
}

func resolutionCandidate(runtime *config.Config, candidates []rules.Decision) (rules.Decision, bool, error) {
	conclusive := candidates[len(candidates)-1]
	route, ok := runtime.Outbounds[conclusive.Outbound]
	if !ok {
		return rules.Decision{}, false, fmt.Errorf("outbound %q is not configured", conclusive.Outbound)
	}
	if route.Type != "reject" {
		return conclusive, true, nil
	}
	for _, candidate := range candidates[:len(candidates)-1] {
		route, ok := runtime.Outbounds[candidate.Outbound]
		if !ok {
			return rules.Decision{}, false, fmt.Errorf("outbound %q is not configured", candidate.Outbound)
		}
		if route.Type != "reject" {
			return candidate, true, nil
		}
	}
	return conclusive, false, nil
}

func resolveConfigured(ctx context.Context, runtime *config.Config, start, domain, family string, timeout time.Duration) ([]netip.Addr, string, error) {
	current := start
	var failures []error
	for current != "" {
		route, ok := runtime.Outbounds[current]
		if !ok {
			return nil, "", fmt.Errorf("outbound %q is not configured", current)
		}
		if route.Type == "reject" {
			return nil, "", errors.Join(append(failures, fmt.Errorf("outbound %q: %w", current, outbound.ErrRejected))...)
		}
		client, err := resolver.NewClient(route.Interface, route.DNS, timeout, runtime.DNS.MaxConcurrent)
		if err == nil {
			if family == "ipv6" {
				var addresses []netip.Addr
				addresses, err = client.LookupIPv6(ctx, domain)
				if err == nil {
					return addresses, current, nil
				}
			} else {
				var addresses []netip.Addr
				addresses, err = client.LookupIPv4(ctx, domain)
				if err == nil {
					return addresses, current, nil
				}
			}
		}
		failures = append(failures, fmt.Errorf("outbound %q resolve %s: %w", current, domain, err))
		if resolver.IsBusinessError(err) || !outbound.IsRecoverable(err) || route.Fallback == "" {
			return nil, "", errors.Join(failures...)
		}
		current = route.Fallback
	}
	return nil, "", errors.Join(failures...)
}

func printExplainResult(result explainResult) {
	fmt.Printf("flow domain=%q protocol=%s port=%d", result.Input.Domain, result.Input.Protocol, result.Input.Port)
	if len(result.Input.IPs) != 0 {
		fmt.Printf(" ips=%s", strings.Join(result.Input.IPs, ","))
	}
	fmt.Println()
	for _, candidate := range result.Candidates {
		state := "conclusive"
		if candidate.DeferredUntilDNS {
			state = "deferred-cidr"
		}
		fmt.Printf("candidate rule=%d outbound=%s state=%s", candidate.ID, candidate.Outbound, state)
		if len(candidate.Domains) != 0 {
			fmt.Printf(" domain=%s", strings.Join(candidate.Domains, ","))
		}
		if len(candidate.DomainSuffixes) != 0 {
			fmt.Printf(" domain_suffix=%s", strings.Join(candidate.DomainSuffixes, ","))
		}
		if len(candidate.DestinationCIDRs) != 0 {
			fmt.Printf(" ip_cidr=%s", strings.Join(candidate.DestinationCIDRs, ","))
		}
		fmt.Println()
	}
	if result.ResolutionOutbound != "" {
		fmt.Printf("resolved via=%s addresses=%s\n", result.ResolutionOutbound, strings.Join(result.ResolvedAddresses, ","))
	}
	if result.FinalDecision != nil {
		selected := result.FinalDecision
		fmt.Printf("final rule=%d outbound=%s type=%s", selected.RuleID, selected.Outbound.Name, selected.Outbound.Type)
		if selected.Outbound.Interface != "" {
			fmt.Printf(" interface=%s", selected.Outbound.Interface)
		}
		fmt.Println()
	} else {
		fmt.Printf("final pending_dns=true candidate_rule=%d candidate_outbound=%s\n", result.CandidateDecision.RuleID, result.CandidateDecision.Outbound.Name)
	}
	if result.Note != "" {
		fmt.Println("note:", result.Note)
	}
}
