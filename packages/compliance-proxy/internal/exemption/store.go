// Package exemption provides an in-memory store for temporary compliance-hook
// exemptions. Exempted traffic still undergoes TLS bump but skips the
// compliance pipeline, allowing admins to unblock false-positive scenarios
// without disabling inspection entirely.
package exemption

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

// Exemption represents a temporary exemption that allows a source IP + target
// host pair to bypass compliance hooks for a limited duration.
type Exemption struct {
	ID         string    `json:"id"`
	SourceIP   string    `json:"sourceIp"`   // IP or CIDR notation
	TargetHost string    `json:"targetHost"` // exact hostname or wildcard (*.example.com)
	ExpiresAt  time.Time `json:"expiresAt"`
	Reason     string    `json:"reason"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	Disabled   bool      `json:"disabled"`
	// EffectiveFrom is when matching may begin (UTC). Zero means immediate.
	EffectiveFrom time.Time `json:"effectiveFrom,omitempty"`
}

// Store is a concurrency-safe in-memory store for temporary exemptions with
// automatic expiry cleanup.
type Store struct {
	mu     sync.RWMutex
	items  map[string]*Exemption // keyed by ID
	logger *slog.Logger
}

// NewStore creates a new empty exemption store.
func NewStore(logger *slog.Logger) *Store {
	return &Store{
		items:  make(map[string]*Exemption),
		logger: logger,
	}
}

// Add creates a new exemption with an auto-generated UUID and computed expiry.
func (s *Store) Add(sourceIP, targetHost string, duration time.Duration, reason, createdBy string) *Exemption {
	now := time.Now()
	e := &Exemption{
		ID:         uuid.NewString(),
		SourceIP:   sourceIP,
		TargetHost: targetHost,
		ExpiresAt:  now.Add(duration),
		Reason:     reason,
		CreatedBy:  createdBy,
		CreatedAt:  now,
	}

	s.mu.Lock()
	s.items[e.ID] = e
	s.mu.Unlock()

	s.logger.Info("exemption added",
		"id", e.ID,
		"sourceIp", sourceIP,
		"targetHost", targetHost,
		"expiresAt", e.ExpiresAt,
		"reason", reason,
		"createdBy", createdBy,
	)

	return e
}

// Rebuild replaces the in-memory exemption set with entries from a shadow
// snapshot. It is the sole mutator called by the Hub shadow-sync path
// (OnConfigChanged → ApplyActiveExemptions). Entries with unparseable
// ExpiresAt or past-expiry are silently dropped — the shadow snapshot is
// authoritative and the proxy's view is best-effort. ApprovedBy maps to
// CreatedBy; CreatedAt is stamped at apply time because shadow entries
// don't carry it.
func (s *Store) Rebuild(entries []identity.ActiveExemption) {
	now := time.Now()
	next := make(map[string]*Exemption, len(entries))
	dropped := 0
	for _, e := range entries {
		expires, err := time.Parse(time.RFC3339, e.ExpiresAt)
		if err != nil {
			dropped++
			continue
		}
		if expires.Before(now) {
			dropped++
			continue
		}
		var eff time.Time
		if e.EffectiveFrom != "" {
			if t, err := time.Parse(time.RFC3339, e.EffectiveFrom); err == nil {
				eff = t
			}
		}
		if !eff.IsZero() && eff.After(now) {
			dropped++
			continue
		}
		if isOverBroadExemption(e.SourceIP, e.TargetHost) {
			dropped++
			s.logger.Warn("exemption dropped: over-broad grant (blank/'*' source AND host) would bypass all compliance",
				"id", e.ID,
				"sourceIp", e.SourceIP,
				"targetHost", e.TargetHost,
			)
			continue
		}
		next[e.ID] = &Exemption{
			ID:            e.ID,
			SourceIP:      e.SourceIP,
			TargetHost:    e.TargetHost,
			ExpiresAt:     expires,
			Reason:        e.Reason,
			CreatedBy:     e.ApprovedBy,
			CreatedAt:     now,
			Disabled:      e.Disabled,
			EffectiveFrom: eff,
		}
	}

	s.mu.Lock()
	s.items = next
	s.mu.Unlock()

	s.logger.Info("exemption store rebuilt from shadow",
		"active", len(next),
		"dropped", dropped,
	)
}

// Remove deletes an exemption by ID. Returns true if the exemption existed.
func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	_, existed := s.items[id]
	if existed {
		delete(s.items, id)
	}
	s.mu.Unlock()

	if existed {
		s.logger.Info("exemption removed", "id", id)
	}
	return existed
}

// Snapshot returns the active (non-expired) exemption list in the shared
// configtypes shape used by the /runtime/config read surface. Timestamps
// are serialised as RFC3339. CreatedBy maps to ApprovedBy because the
// external shape treats the "author" as the approver.
func (s *Store) Snapshot() identity.ActiveExemptions {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := identity.ActiveExemptions{
		Entries: make([]identity.ActiveExemption, 0, len(s.items)),
	}
	for _, e := range s.items {
		if e.ExpiresAt.Before(now) {
			continue
		}
		ae := identity.ActiveExemption{
			ID:         e.ID,
			SourceIP:   e.SourceIP,
			TargetHost: e.TargetHost,
			ExpiresAt:  e.ExpiresAt.Format(time.RFC3339),
			Reason:     e.Reason,
			ApprovedBy: e.CreatedBy,
			Disabled:   e.Disabled,
		}
		if !e.EffectiveFrom.IsZero() {
			ae.EffectiveFrom = e.EffectiveFrom.UTC().Format(time.RFC3339)
		}
		out.Entries = append(out.Entries, ae)
	}
	return out
}

// List returns all active (non-expired) exemptions.
func (s *Store) List() []*Exemption {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Exemption, 0, len(s.items))
	for _, e := range s.items {
		if e.ExpiresAt.After(now) {
			result = append(result, e)
		}
	}
	return result
}

// IsExempt checks whether a given sourceIP and targetHost match any active
// exemption. It supports CIDR matching for sourceIP and wildcard matching for
// targetHost (e.g. "*.openai.com" matches "api.openai.com").
// Returns the matched exemption if found.
func (s *Store) IsExempt(sourceIP, targetHost string) (bool, *Exemption) {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, e := range s.items {
		if e.ExpiresAt.Before(now) {
			continue
		}
		if !e.EffectiveFrom.IsZero() && e.EffectiveFrom.After(now) {
			continue
		}
		if e.Disabled {
			continue
		}
		// Module-local floor: never honour an all-matching grant, even if one
		// slipped past Rebuild.
		if isOverBroadExemption(e.SourceIP, e.TargetHost) {
			continue
		}
		if !matchSourceIP(e.SourceIP, sourceIP) {
			continue
		}
		if !matchTargetHost(e.TargetHost, targetHost) {
			continue
		}
		return true, e
	}
	return false, nil
}

// StartCleanup launches a background goroutine that periodically removes
// expired exemptions. It stops when the context is cancelled.
func (s *Store) StartCleanup(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.purgeExpired()
			}
		}
	}()
}

// purgeExpired removes all expired exemptions from the store.
func (s *Store) purgeExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, e := range s.items {
		if e.ExpiresAt.Before(now) {
			delete(s.items, id)
			removed++
		}
	}
	if removed > 0 {
		s.logger.Debug("expired exemptions purged", "count", removed)
	}
}

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
func matchSourceIP(spec, clientIP string) bool {
	// Empty spec matches everything.
	if spec == "" || spec == "*" {
		return true
	}

	// Try CIDR match first.
	if strings.Contains(spec, "/") {
		_, cidr, err := net.ParseCIDR(spec)
		if err != nil {
			return false
		}
		ip := net.ParseIP(clientIP)
		if ip == nil {
			return false
		}
		return cidr.Contains(ip)
	}

	// Exact IP match.
	specIP := net.ParseIP(spec)
	cIP := net.ParseIP(clientIP)
	if specIP == nil || cIP == nil {
		return false
	}
	return specIP.Equal(cIP)
}

// matchTargetHost checks if the request host matches the exemption's target
// specification. Supports exact match and wildcard prefix (*.example.com).
func matchTargetHost(spec, host string) bool {
	if spec == "" || spec == "*" {
		return true
	}

	specLower := strings.ToLower(spec)
	hostLower := strings.ToLower(host)

	// Wildcard match: *.example.com matches api.example.com, sub.api.example.com
	// but NOT the apex domain example.com itself (standard wildcard semantics).
	if strings.HasPrefix(specLower, "*.") {
		suffix := specLower[1:] // ".example.com"
		return strings.HasSuffix(hostLower, suffix)
	}

	// Exact match.
	return specLower == hostLower
}
