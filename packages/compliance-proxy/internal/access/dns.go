package access

import (
	"context"
	"fmt"
	"net"
)

// Resolver is the DNS lookup interface used by PrivateIPChecker.
// net.DefaultResolver satisfies this interface; tests may inject a stub via
// WithResolver to avoid real network calls.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// PrivateIPChecker resolves hostnames and rejects connections to private/reserved IP ranges.
type PrivateIPChecker struct {
	reservedNets []*net.IPNet
	exceptions   []*net.IPNet
	resolver     Resolver
}

// SetResolverForTest swaps the underlying DNS resolver. Intended only for
// tests — production code MUST NOT call this. Wired in to keep test runs
// hermetic (no outbound DNS).
func (p *PrivateIPChecker) SetResolverForTest(r Resolver) {
	p.resolver = r
}

// defaultReservedCIDRs lists all private/reserved ranges per RFC 1918, 6598, and related.
var defaultReservedCIDRs = []string{
	// IPv4
	"10.0.0.0/8",     // RFC 1918
	"172.16.0.0/12",  // RFC 1918
	"192.168.0.0/16", // RFC 1918
	"127.0.0.0/8",    // Loopback
	"169.254.0.0/16", // Link-local
	"100.64.0.0/10",  // RFC 6598 CGN
	// IPv6
	"::1/128",   // Loopback
	"fc00::/7",  // Unique local
	"fe80::/10", // Link-local
}

// PrivateIPCheckerOption is a functional option for NewPrivateIPChecker.
type PrivateIPCheckerOption func(*PrivateIPChecker)

// WithResolver overrides the DNS resolver used by the checker.
// The primary use case is test injection: pass a stub resolver to avoid
// real network calls. Production callers omit this option and get
// net.DefaultResolver.
func WithResolver(r Resolver) PrivateIPCheckerOption {
	return func(p *PrivateIPChecker) {
		if r != nil {
			p.resolver = r
		}
	}
}

// NewPrivateIPChecker creates a checker with all standard private/reserved ranges.
// exceptionCIDRs are admin-configured ranges for internal AI services that should be allowed.
func NewPrivateIPChecker(exceptionCIDRs []string, opts ...PrivateIPCheckerOption) (*PrivateIPChecker, error) {
	reserved := make([]*net.IPNet, 0, len(defaultReservedCIDRs))
	for _, cidr := range defaultReservedCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			// This should never happen with our hardcoded CIDRs.
			return nil, fmt.Errorf("internal error parsing reserved CIDR %q: %w", cidr, err)
		}
		reserved = append(reserved, ipNet)
	}

	exceptions := make([]*net.IPNet, 0, len(exceptionCIDRs))
	for _, cidr := range exceptionCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid exception CIDR %q: %w", cidr, err)
		}
		exceptions = append(exceptions, ipNet)
	}

	p := &PrivateIPChecker{
		reservedNets: reserved,
		exceptions:   exceptions,
		// Default to a caching resolver so a connection burst to one host
		// collapses to a single DNS lookup. WithResolver / SetResolverForTest
		// replace this with a raw (uncached) resolver for hermetic tests.
		resolver: newCachingResolver(net.DefaultResolver, dnsCacheTTL),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// CheckResolved checks if any of the given IPs are in private/reserved ranges (excluding exceptions).
// Returns error if any IP is private.
func (p *PrivateIPChecker) CheckResolved(ips []net.IP) error {
	for _, ip := range ips {
		if p.isReserved(ip) && !p.isException(ip) {
			return fmt.Errorf("%w: %s", ErrPrivateIP, ip.String())
		}
	}
	return nil
}

// ResolveAndCheck resolves the hostname and checks all resolved IPs.
func (p *PrivateIPChecker) ResolveAndCheck(ctx context.Context, host string) error {
	addrs, err := p.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("DNS resolution failed for %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("DNS resolution returned no addresses for %q", host)
	}

	ips := make([]net.IP, len(addrs))
	for i, addr := range addrs {
		ips[i] = addr.IP
	}
	return p.CheckResolved(ips)
}

func (p *PrivateIPChecker) isReserved(ip net.IP) bool {
	for _, n := range p.reservedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (p *PrivateIPChecker) isException(ip net.IP) bool {
	for _, n := range p.exceptions {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
