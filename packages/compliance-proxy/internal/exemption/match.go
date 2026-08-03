// Package exemption's matching predicates: the pure functions that decide
// whether a stored exemption applies to a given flow. Split out of store.go so
// the store file carries lifecycle (add / rebuild / snapshot / purge) and this
// one carries the match semantics — the two change for different reasons, and
// the match rules are what a reviewer needs to read in isolation when auditing
// who gets exempted from compliance inspection.
package exemption

import (
	"net"
	"strings"
)

// isOverBroadExemption reports whether an exemption would match EVERY flow —
// BOTH its source-IP and target-host selectors are blank or the catch-all "*".
// Such a grant sets hookExempted=true for all traffic and skips the entire
// compliance pipeline. The store refuses it at Rebuild (logged +
// counted as dropped) AND the IsExempt hot path treats it as a never-match
// floor even if one somehow entered the map, mirroring the access layer's
// NewDomainAllowlist, which rejects an open wildcard at construction. A grant
// must scope at least one dimension (a source IP/CIDR or a target host).
//
// Zero-prefix CIDRs (0.0.0.0/0, ::/0) match every IPv4/IPv6 address and are
// therefore also treated as blank on the source dimension.
func isOverBroadExemption(sourceIP, targetHost string) bool {
	blank := func(s string) bool { return s == "" || s == "*" }
	catchAllCIDR := func(s string) bool {
		if !strings.Contains(s, "/") {
			return false
		}
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			return false
		}
		ones, _ := cidr.Mask.Size()
		return ones == 0
	}
	return (blank(sourceIP) || catchAllCIDR(sourceIP)) && blank(targetHost)
}

// matchSourceIP checks if clientIP matches the exemption's source specification.
// The spec can be a single IP ("10.0.0.5") or a CIDR range ("10.0.0.0/24").
//
// Kept as the string-in convenience form for callers with a single spec to
// test; the hot path (IsExempt, which tests one client against every stored
// exemption) uses matchSourceIPParsed so the client IP is parsed once per
// request rather than once per exemption.
func matchSourceIP(spec, clientIP string) bool {
	return matchSourceIPParsed(spec, net.ParseIP(clientIP))
}

// matchSourceIPParsed is matchSourceIP with the client IP already parsed.
// A nil clientIP means the caller could not parse it; an empty/"*" spec still
// matches everything (that check is independent of the client address), but
// any spec requiring a comparison fails closed.
func matchSourceIPParsed(spec string, clientIP net.IP) bool {
	// Empty spec matches everything.
	if spec == "" || spec == "*" {
		return true
	}
	if clientIP == nil {
		return false
	}

	// Try CIDR match first.
	if strings.Contains(spec, "/") {
		_, cidr, err := net.ParseCIDR(spec)
		if err != nil {
			return false
		}
		return cidr.Contains(clientIP)
	}

	// Exact IP match.
	specIP := net.ParseIP(spec)
	if specIP == nil {
		return false
	}
	return specIP.Equal(clientIP)
}

// matchTargetHost checks if the request host matches the exemption's target
// specification. Supports exact match and wildcard prefix (*.example.com).
//
// Convenience form for callers with a single spec to test; the hot path
// (IsExempt) uses matchTargetHostLowered so the request host is lowercased
// once per request rather than once per exemption.
func matchTargetHost(spec, host string) bool {
	return matchTargetHostLowered(spec, strings.ToLower(host))
}

// matchTargetHostLowered is matchTargetHost with the request host already
// lowercased. The spec is still lowercased per call — it is per-exemption
// constant and could be precomputed at Add/Rebuild time, but that requires
// carrying derived state on Exemption; see the register's follow-up note.
func matchTargetHostLowered(spec, hostLower string) bool {
	if spec == "" || spec == "*" {
		return true
	}

	specLower := strings.ToLower(spec)

	// Wildcard match: *.example.com matches api.example.com, sub.api.example.com
	// but NOT the apex domain example.com itself (standard wildcard semantics).
	if strings.HasPrefix(specLower, "*.") {
		suffix := specLower[1:] // ".example.com"
		return strings.HasSuffix(hostLower, suffix)
	}

	// Exact match.
	return specLower == hostLower
}
