package access

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
)

// Sentinel errors for access control rejections.
var (
	ErrIPDenied     = errors.New("source IP not in allowlist")
	ErrDomainDenied = errors.New("destination not in domain allowlist")
	ErrPrivateIP    = errors.New("resolved IP is in private/reserved range")
	ErrSNIMismatch  = errors.New("SNI does not match CONNECT target")
)

// Checker combines all access control checks. The domain allowlist can
// be hot-swapped at runtime via SwapDomainAllowlist (DB-driven
// InterceptionDomain changes). The source IP allowlist is yaml-only
// (boot-fixed) — see compliance-proxy.yaml `accessControl:` block.
type Checker struct {
	ipAllowlist    atomic.Pointer[IPAllowlist]
	domainAL       atomic.Pointer[DomainAllowlist]
	staticEntries  []string // YAML-configured entries that are always included
	privateIPCheck *PrivateIPChecker
}

// NewChecker creates a combined access checker from config slices.
// opts are forwarded to NewPrivateIPChecker and can be used to inject a
// test resolver via WithResolver so unit tests avoid real DNS lookups.
func NewChecker(ipCIDRs, domainEntries, exceptionCIDRs []string, opts ...PrivateIPCheckerOption) (*Checker, error) {
	ipAL, err := NewIPAllowlist(ipCIDRs)
	if err != nil {
		return nil, err
	}

	domAL, err := NewDomainAllowlist(domainEntries)
	if err != nil {
		return nil, err
	}

	privCheck, err := NewPrivateIPChecker(exceptionCIDRs, opts...)
	if err != nil {
		return nil, err
	}

	c := &Checker{
		staticEntries:  domainEntries,
		privateIPCheck: privCheck,
	}
	c.ipAllowlist.Store(ipAL)
	c.domainAL.Store(domAL)
	return c, nil
}

// SwapDomainAllowlist merges the static YAML entries with dynamic DB entries
// and atomically replaces the active domain allowlist. This is called by
// the thingclient.OnConfigChanged callback after Hub broadcasts an
// InterceptionDomain change over the WebSocket control channel.
func (c *Checker) SwapDomainAllowlist(dbEntries []string, logger *slog.Logger) {
	// Merge: static YAML entries + dynamic DB entries (deduped).
	seen := make(map[string]bool, len(c.staticEntries)+len(dbEntries))
	merged := make([]string, 0, len(c.staticEntries)+len(dbEntries))
	for _, e := range c.staticEntries {
		if !seen[e] {
			seen[e] = true
			merged = append(merged, e)
		}
	}
	for _, e := range dbEntries {
		if !seen[e] {
			seen[e] = true
			merged = append(merged, e)
		}
	}

	newAL, err := NewDomainAllowlist(merged)
	if err != nil {
		logger.Warn("failed to build domain allowlist from DB entries, keeping current",
			slog.String("error", err.Error()),
			slog.Int("staticCount", len(c.staticEntries)),
			slog.Int("dbCount", len(dbEntries)),
		)
		return
	}

	c.domainAL.Store(newAL)
	logger.Info("domain allowlist hot-swapped",
		slog.Int("staticCount", len(c.staticEntries)),
		slog.Int("dbCount", len(dbEntries)),
		slog.Int("totalCount", len(merged)),
	)
}

// SetResolverForTest swaps the embedded PrivateIPChecker's resolver.
// Intended only for tests — production code MUST NOT call this. Used by
// proxy/server tests to keep CheckConnect hermetic (no outbound DNS for
// "example.com" or similar).
func (c *Checker) SetResolverForTest(r Resolver) {
	c.privateIPCheck.SetResolverForTest(r)
}

// CheckConnect runs all pre-tunnel checks: IP allowlist, domain allowlist, DNS private IP.
// Returns a structured error with reason for rejection metrics.
func (c *Checker) CheckConnect(ctx context.Context, sourceIP net.IP, host, port string) error {
	if !c.ipAllowlist.Load().Allow(sourceIP) {
		return ErrIPDenied
	}

	if !c.domainAL.Load().Allow(host, port) {
		return ErrDomainDenied
	}

	if err := c.privateIPCheck.ResolveAndCheck(ctx, host); err != nil {
		return err
	}

	return nil
}
