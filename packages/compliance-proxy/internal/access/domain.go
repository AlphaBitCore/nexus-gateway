package access

import (
	"fmt"
	"strings"
)

// DomainAllowlist checks if a destination host:port is allowed.
type DomainAllowlist struct {
	exact     map[string]bool // "api.openai.com:443" → true
	wildcards []wildcardEntry // "*.openai.com:443"
}

type wildcardEntry struct {
	suffix string // ".openai.com"
	port   string // "443"
}

// NewDomainAllowlist parses host:port entries. Prefix wildcards (*.example.com:443) are supported.
// Open wildcards (*:443 or *) are rejected with an error.
func NewDomainAllowlist(entries []string) (*DomainAllowlist, error) {
	d := &DomainAllowlist{
		exact: make(map[string]bool, len(entries)),
	}
	for _, entry := range entries {
		host, port, err := splitHostPort(entry)
		if err != nil {
			return nil, err
		}
		host = strings.ToLower(host)

		if host == "*" {
			return nil, fmt.Errorf("open wildcard %q is not allowed", entry)
		}

		if strings.HasPrefix(host, "*.") {
			// Extract the suffix after the "*" — keep the leading dot.
			suffix := host[1:] // e.g. ".openai.com"
			d.wildcards = append(d.wildcards, wildcardEntry{suffix: suffix, port: port})
		} else {
			d.exact[host+":"+port] = true
		}
	}
	return d, nil
}

// Allow returns true if host:port is in the allowlist.
func (d *DomainAllowlist) Allow(host, port string) bool {
	host = strings.ToLower(host)

	// Exact match.
	if d.exact[host+":"+port] {
		return true
	}

	// Wildcard match: *.openai.com:443 matches sub.openai.com:443
	// but NOT openai.com:443 (the suffix is ".openai.com", so the host
	// must have at least one character before the dot).
	for _, w := range d.wildcards {
		if port != w.port {
			continue
		}
		if strings.HasSuffix(host, w.suffix) && len(host) > len(w.suffix) {
			return true
		}
	}
	return false
}

// splitHostPort splits "host:port" with a default port of "443" when omitted.
func splitHostPort(entry string) (host, port string, err error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", "", fmt.Errorf("empty domain entry")
	}

	// Check for IPv6 bracket notation (unlikely for domain allowlist, but be safe).
	if strings.HasPrefix(entry, "[") {
		idx := strings.LastIndex(entry, "]")
		if idx == -1 {
			return "", "", fmt.Errorf("invalid bracketed entry %q", entry)
		}
		host = entry[1:idx]
		rest := entry[idx+1:]
		if rest == "" {
			return host, "443", nil
		}
		if rest[0] != ':' {
			return "", "", fmt.Errorf("invalid entry %q: expected colon after bracket", entry)
		}
		return host, rest[1:], nil
	}

	// For wildcard or domain entries, the last colon separates host and port.
	idx := strings.LastIndex(entry, ":")
	if idx == -1 {
		return entry, "443", nil
	}

	return entry[:idx], entry[idx+1:], nil
}
