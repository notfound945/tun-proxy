// Package domainname provides one canonical representation shared by config,
// Fake DNS, the address pool and rules.
package domainname

import (
	"fmt"
	"net/netip"
	"strings"
)

type Pattern struct {
	Suffix bool
	Domain string
}

func Normalize(value string) (string, error) {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if domain == "" || len(domain) > 253 {
		return "", fmt.Errorf("invalid domain %q", value)
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return "", fmt.Errorf("IP literal %q is not a domain", value)
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid domain %q", value)
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '_' {
				return "", fmt.Errorf("invalid domain %q", value)
			}
		}
	}
	return domain, nil
}

func ParsePattern(value string) (Pattern, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	pattern := Pattern{}
	if strings.HasPrefix(value, "*.") {
		pattern.Suffix = true
		value = strings.TrimPrefix(value, "*.")
	}
	domain, err := Normalize(value)
	if err != nil {
		return Pattern{}, err
	}
	pattern.Domain = domain
	return pattern, nil
}

func (pattern Pattern) Matches(normalizedDomain string) bool {
	if pattern.Suffix {
		return normalizedDomain == pattern.Domain || strings.HasSuffix(normalizedDomain, "."+pattern.Domain)
	}
	return normalizedDomain == pattern.Domain
}
