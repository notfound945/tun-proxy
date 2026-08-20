package session

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/hailinpan/tun-proxy/internal/outbound"
	"github.com/hailinpan/tun-proxy/internal/resolver"
	"github.com/hailinpan/tun-proxy/internal/rules"
)

// routeResolution freezes the addresses obtained through one actual outbound.
// The policy decision remains separate because resolver fallback may use a
// different route without changing the matched rule.
type routeResolution struct {
	Outbound  string
	Addresses []netip.Addr
}

func decideFlow(
	ctx context.Context,
	matcher Matcher,
	routes map[string]Route,
	metadata rules.FlowMetadata,
	domain string,
	literal netip.Addr,
	ipv6 bool,
	onFallback func(),
) (rules.Decision, *routeResolution, error) {
	if literal.IsValid() {
		decision, err := matcher.MatchResolved(metadata, []netip.Addr{literal})
		return decision, nil, err
	}

	candidates, err := matcher.Candidates(metadata)
	if err != nil {
		return rules.Decision{}, nil, err
	}
	candidate := candidates[len(candidates)-1]
	resolutionCandidate := candidate
	candidateRoute, ok := routes[resolutionCandidate.Outbound]
	if !ok {
		return candidate, nil, fmt.Errorf("outbound %q is not configured", resolutionCandidate.Outbound)
	}
	if candidateRoute.Reject {
		for _, deferred := range candidates[:len(candidates)-1] {
			route, exists := routes[deferred.Outbound]
			if !exists {
				return candidate, nil, fmt.Errorf("outbound %q is not configured", deferred.Outbound)
			}
			if !route.Reject {
				resolutionCandidate = deferred
				candidateRoute = route
				break
			}
		}
		if candidateRoute.Reject {
			// Every rule that can win before or at the conclusive reject is
			// also reject-only, so resolution cannot change the result.
			return candidate, nil, nil
		}
	}

	resolution, err := resolveForDecision(ctx, routes, resolutionCandidate.Outbound, domain, ipv6, onFallback)
	if err != nil {
		return candidate, nil, err
	}
	final, err := matcher.MatchResolved(metadata, resolution.Addresses)
	if err != nil {
		return rules.Decision{}, nil, err
	}
	if final.Outbound == resolutionCandidate.Outbound || final.Outbound == resolution.Outbound {
		return final, resolution, nil
	}
	finalRoute, ok := routes[final.Outbound]
	if !ok {
		return final, nil, fmt.Errorf("outbound %q is not configured", final.Outbound)
	}
	if finalRoute.Reject {
		return final, nil, nil
	}

	// DNS caches and socket binding are isolated by outbound, so a policy
	// switch must resolve again through the selected outbound. The rule is not
	// re-matched against this second answer set: one flow owns one immutable
	// decision.
	resolution, err = resolveForDecision(ctx, routes, final.Outbound, domain, ipv6, onFallback)
	return final, resolution, err
}

func resolveForDecision(
	ctx context.Context,
	routes map[string]Route,
	start string,
	domain string,
	ipv6 bool,
	onFallback func(),
) (*routeResolution, error) {
	current := start
	var failures []error
	for current != "" {
		route, ok := routes[current]
		if !ok {
			return nil, fmt.Errorf("outbound %q is not configured", current)
		}
		if route.Reject {
			return nil, errors.Join(append(failures, fmt.Errorf("outbound %q: %w", current, outbound.ErrRejected))...)
		}
		addresses, err := lookupAddresses(ctx, route.Resolver, domain, ipv6)
		if err == nil && len(addresses) != 0 {
			return &routeResolution{Outbound: current, Addresses: append([]netip.Addr(nil), addresses...)}, nil
		}
		if err == nil {
			err = errors.New("resolver returned no addresses")
		}
		failures = append(failures, fmt.Errorf("outbound %q resolve %s: %w", current, domain, err))
		if resolver.IsBusinessError(err) || !outbound.IsRecoverable(err) || route.Fallback == "" {
			return nil, errors.Join(failures...)
		}
		if onFallback != nil {
			onFallback()
		}
		current = route.Fallback
	}
	return nil, errors.Join(failures...)
}
