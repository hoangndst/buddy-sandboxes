package egressgateway

import (
	"net"
	"strings"
)

// MatchDomain checks whether the requested host matches any pattern in allowedDomains.
// Matches exact domains (e.g. "github.com") and wildcard domains (e.g. "*.npmjs.org").
// Wildcards match both subdomains (e.g. "registry.npmjs.org") and the apex domain ("npmjs.org").
func MatchDomain(host string, allowedDomains []string) bool {
	if len(allowedDomains) == 0 {
		return false
	}

	cleanHost := strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(cleanHost); err == nil {
		cleanHost = h
	}
	cleanHost = strings.TrimSuffix(strings.ToLower(cleanHost), ".")

	for _, pattern := range allowedDomains {
		cleanPattern := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
		if cleanPattern == cleanHost {
			return true
		}
		if strings.HasPrefix(cleanPattern, "*.") {
			root := cleanPattern[2:]
			if cleanHost == root || strings.HasSuffix(cleanHost, "."+root) {
				return true
			}
		}
	}
	return false
}
