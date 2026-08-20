package resolver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/domainname"
	"github.com/miekg/dns"
)

const maxCacheEntries = 1024

var (
	ErrNoIPv4Address = errors.New("DNS response contains no IPv4 address")
	ErrNoIPv6Address = errors.New("DNS response contains no IPv6 address")
)

type ResponseError struct {
	Domain string
	RCode  int
}

func (err *ResponseError) Error() string {
	return fmt.Sprintf("DNS response for %s returned %s", err.Domain, dns.RcodeToString[err.RCode])
}

type cacheEntry struct {
	addresses []netip.Addr
	expiresAt time.Time
}

type cacheKey struct {
	domain string
	qtype  uint16
}

// LookupIPv4 resolves all A records using this client's interface-bound
// upstreams. Its TTL cache belongs to this Client, so answers never cross an
// outbound boundary.
func (client *Client) LookupIPv4(ctx context.Context, domain string) ([]netip.Addr, error) {
	normalized, err := domainname.Normalize(domain)
	if err != nil {
		return nil, err
	}
	if addresses := client.cached(normalized, dns.TypeA, time.Now()); len(addresses) != 0 {
		return addresses, nil
	}

	request := new(dns.Msg)
	request.SetQuestion(dns.Fqdn(normalized), dns.TypeA)
	reply, err := client.Exchange(ctx, request)
	if err != nil {
		return nil, err
	}
	if reply.Rcode != dns.RcodeSuccess {
		return nil, &ResponseError{Domain: normalized, RCode: reply.Rcode}
	}

	target, ttl, haveTTL, err := answerTarget(reply.Answer, normalized)
	if err != nil {
		return nil, err
	}
	seen := make(map[netip.Addr]struct{})
	addresses := make([]netip.Addr, 0, len(reply.Answer))
	for _, answer := range reply.Answer {
		record, ok := answer.(*dns.A)
		if !ok || canonicalDNSName(record.Hdr.Name) != target {
			continue
		}
		address, ok := netip.AddrFromSlice(record.A.To4())
		if !ok || !address.Is4() {
			continue
		}
		ttl, haveTTL = minimumTTL(ttl, haveTTL, record.Hdr.Ttl)
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w for %s", ErrNoIPv4Address, normalized)
	}
	if haveTTL && ttl > 0 {
		client.store(normalized, dns.TypeA, addresses, time.Now().Add(time.Duration(ttl)*time.Second))
	}
	return append([]netip.Addr(nil), addresses...), nil
}

// LookupIPv6 resolves all AAAA records using this client's interface-bound
// upstreams. A and AAAA answers use separate cache entries so one family can
// never satisfy a lookup for the other.
func (client *Client) LookupIPv6(ctx context.Context, domain string) ([]netip.Addr, error) {
	normalized, err := domainname.Normalize(domain)
	if err != nil {
		return nil, err
	}
	if addresses := client.cached(normalized, dns.TypeAAAA, time.Now()); len(addresses) != 0 {
		return addresses, nil
	}

	request := new(dns.Msg)
	request.SetQuestion(dns.Fqdn(normalized), dns.TypeAAAA)
	reply, err := client.Exchange(ctx, request)
	if err != nil {
		return nil, err
	}
	if reply.Rcode != dns.RcodeSuccess {
		return nil, &ResponseError{Domain: normalized, RCode: reply.Rcode}
	}

	target, ttl, haveTTL, err := answerTarget(reply.Answer, normalized)
	if err != nil {
		return nil, err
	}
	seen := make(map[netip.Addr]struct{})
	addresses := make([]netip.Addr, 0, len(reply.Answer))
	for _, answer := range reply.Answer {
		record, ok := answer.(*dns.AAAA)
		if !ok || canonicalDNSName(record.Hdr.Name) != target {
			continue
		}
		address, ok := netip.AddrFromSlice(record.AAAA)
		if !ok || !address.Is6() || address.Is4In6() {
			continue
		}
		ttl, haveTTL = minimumTTL(ttl, haveTTL, record.Hdr.Ttl)
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w for %s", ErrNoIPv6Address, normalized)
	}
	if haveTTL && ttl > 0 {
		client.store(normalized, dns.TypeAAAA, addresses, time.Now().Add(time.Duration(ttl)*time.Second))
	}
	return append([]netip.Addr(nil), addresses...), nil
}

func answerTarget(answers []dns.RR, domain string) (string, uint32, bool, error) {
	type cnameRecord struct {
		target   string
		ttl      uint32
		conflict bool
	}
	cnames := make(map[string]cnameRecord)
	for _, answer := range answers {
		record, ok := answer.(*dns.CNAME)
		if !ok {
			continue
		}
		owner := canonicalDNSName(record.Hdr.Name)
		target := canonicalDNSName(record.Target)
		if existing, exists := cnames[owner]; exists {
			if existing.target != target {
				existing.conflict = true
			}
			existing.ttl, _ = minimumTTL(existing.ttl, true, record.Hdr.Ttl)
			cnames[owner] = existing
			continue
		}
		cnames[owner] = cnameRecord{target: target, ttl: record.Hdr.Ttl}
	}

	current := canonicalDNSName(domain)
	visited := make(map[string]struct{})
	var ttl uint32
	haveTTL := false
	for {
		if _, exists := visited[current]; exists {
			return "", 0, false, fmt.Errorf("DNS response contains a CNAME loop at %s", current)
		}
		visited[current] = struct{}{}
		record, exists := cnames[current]
		if !exists {
			return current, ttl, haveTTL, nil
		}
		if record.conflict {
			return "", 0, false, fmt.Errorf("DNS response contains conflicting CNAME targets for %s", current)
		}
		ttl, haveTTL = minimumTTL(ttl, haveTTL, record.ttl)
		current = record.target
	}
}

func canonicalDNSName(name string) string {
	return strings.ToLower(dns.Fqdn(name))
}

func minimumTTL(current uint32, haveCurrent bool, candidate uint32) (uint32, bool) {
	if !haveCurrent || candidate < current {
		return candidate, true
	}
	return current, true
}

func (client *Client) cached(domain string, qtype uint16, now time.Time) []netip.Addr {
	client.cacheMutex.Lock()
	defer client.cacheMutex.Unlock()
	key := cacheKey{domain: domain, qtype: qtype}
	entry, ok := client.cache[key]
	if !ok {
		return nil
	}
	if !now.Before(entry.expiresAt) {
		delete(client.cache, key)
		return nil
	}
	return append([]netip.Addr(nil), entry.addresses...)
}

func (client *Client) store(domain string, qtype uint16, addresses []netip.Addr, expiresAt time.Time) {
	client.cacheMutex.Lock()
	defer client.cacheMutex.Unlock()
	if len(client.cache) >= maxCacheEntries {
		var oldestKey cacheKey
		haveOldest := false
		var oldestExpiry time.Time
		for candidate, entry := range client.cache {
			if !haveOldest || entry.expiresAt.Before(oldestExpiry) {
				oldestKey = candidate
				oldestExpiry = entry.expiresAt
				haveOldest = true
			}
		}
		delete(client.cache, oldestKey)
	}
	client.cache[cacheKey{domain: domain, qtype: qtype}] = cacheEntry{
		addresses: append([]netip.Addr(nil), addresses...),
		expiresAt: expiresAt,
	}
}

// IsBusinessError identifies DNS answers for which switching interfaces must
// not silently change policy behavior.
func IsBusinessError(err error) bool {
	var responseErr *ResponseError
	if errors.As(err, &responseErr) {
		return !isRetryableRCode(responseErr.RCode)
	}
	return errors.Is(err, ErrNoIPv4Address) || errors.Is(err, ErrNoIPv6Address)
}
