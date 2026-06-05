package local

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateBaseURL rejects a Control Plane / AI Gateway base URL that the CLI
// could never reach. It exists because the env wizard and `nexus env add`
// previously stored whatever string the operator typed: a value like
// "https://prod" (a placeholder mistaken for the env name) is a syntactically
// valid URL whose host does not resolve, so every later admin call failed deep
// in the request path with a cryptic "dial tcp: lookup prod: no such host"
// instead of a clear error at entry.
//
// field is the human label used in the error (e.g. "Control Plane URL").
// Accepted: an absolute http(s) URL whose host is localhost, an IP, or a
// dotted name (a real FQDN). A bare single-label host ("prod", "gateway") is
// rejected — it cannot resolve from an operator's machine and is almost always
// a typo.
func ValidateBaseURL(field, raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must start with http:// or https:// (got %q)", field, raw)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%s is missing a host (got %q)", field, raw)
	}
	if !hostLooksReachable(host) {
		return fmt.Errorf("%s host %q is not reachable — use a full domain (e.g. https://nexus.example.com), localhost, or an IP", field, host)
	}
	return nil
}

// hostLooksReachable reports whether host is something an operator's machine can
// plausibly resolve: localhost, a literal IP, or a dotted name. A bare label
// (no dot) like "prod" is treated as unreachable.
func hostLooksReachable(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return strings.Contains(host, ".")
}
