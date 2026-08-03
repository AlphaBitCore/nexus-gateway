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
	"sync/atomic"
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
//
// The read path takes NO LOCK. IsExempt runs on every bumped request while the
// writers — a Hub config push (Rebuild), an admin grant/revoke (Add/Remove) and a
// periodic purge — are rare, so the store is copy-on-write behind an
// atomic.Pointer, the same shape `policy/domain.Engine` and
// `transport/streaming/policy.Store` already use for their per-request reads.
//
// This replaced a sync.RWMutex. Reducing the read path to one acquisition was not
// enough: sync.RWMutex blocks new readers once a writer is queued, so a config
// push could stall every in-flight interception behind its own critical section.
// An atomic load cannot be blocked by a writer at all.
//
// writeMu serialises WRITERS ONLY — each one reads the current snapshot, builds a
// new one and swaps it, which is a read-modify-write that two concurrent writers
// would otherwise lose. Readers never touch it, so the hot path stays lock-free
// (B13: no new mutex on a hot path; this removes one).
//
// Snapshots share their *Exemption pointers, which is safe because nothing mutates
// a stored Exemption in place — every writer builds new values. Anything that
// starts mutating one must copy it first, or a reader will observe a torn struct.
type Store struct {
	current atomic.Pointer[snapshot]
	writeMu sync.Mutex
	logger  *slog.Logger
}

// snapshot is an immutable exemption set. Iteration order IS the match order, so
// unlike the map this replaced, IsExempt's first-match result is deterministic —
// see the note on Rebuild.
type snapshot struct {
	items []*Exemption
}

// NewStore creates a new empty exemption store.
func NewStore(logger *slog.Logger) *Store {
	s := &Store{logger: logger}
	s.current.Store(&snapshot{})
	return s
}

// load returns the current snapshot, never nil.
func (s *Store) load() *snapshot { return s.current.Load() }

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

	s.writeMu.Lock()
	cur := s.load().items
	next := make([]*Exemption, 0, len(cur)+1)
	next = append(next, cur...)
	next = append(next, e)
	s.current.Store(&snapshot{items: next})
	s.writeMu.Unlock()

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
	next := make([]*Exemption, 0, len(entries))
	// The map-keyed build this replaced deduplicated by ID implicitly. A slice must
	// do it explicitly, or a shadow snapshot carrying the same ID twice would be
	// scanned twice on every request and could be attributed twice in the audit row.
	seen := make(map[string]struct{}, len(entries))
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
		if _, dup := seen[e.ID]; dup {
			dropped++
			continue
		}
		seen[e.ID] = struct{}{}
		next = append(next, &Exemption{
			ID:            e.ID,
			SourceIP:      e.SourceIP,
			TargetHost:    e.TargetHost,
			ExpiresAt:     expires,
			Reason:        e.Reason,
			CreatedBy:     e.ApprovedBy,
			CreatedAt:     now,
			Disabled:      e.Disabled,
			EffectiveFrom: eff,
		})
	}

	// Held across the publish because Rebuild IS a writer, and writeMu is the
	// lock every other writer takes. Without it a lost update is possible and its
	// direction is the dangerous one: purgeExpired reads the snapshot, filters the
	// expired entries, then stores — so a Hub revocation push landing in that
	// window is overwritten and the revoked exemption goes LIVE again, keeping the
	// whole compliance pipeline bypassed for that source/host pair until something
	// else happens to rewrite the snapshot. -race cannot see it: the pointer swap
	// is atomic, so this is a lost update, not a data race.
	//
	// The build above is deliberately outside the lock — it parses timestamps and
	// allocates, and writeMu exists to serialise the read-modify-WRITE, not the work.
	s.writeMu.Lock()
	s.current.Store(&snapshot{items: next})
	s.writeMu.Unlock()

	s.logger.Info("exemption store rebuilt from shadow",
		"active", len(next),
		"dropped", dropped,
	)
}

// Remove deletes an exemption by ID. Returns true if the exemption existed.
func (s *Store) Remove(id string) bool {
	s.writeMu.Lock()
	cur := s.load().items
	next := make([]*Exemption, 0, len(cur))
	existed := false
	for _, e := range cur {
		if e.ID == id {
			existed = true
			continue
		}
		next = append(next, e)
	}
	if existed {
		s.current.Store(&snapshot{items: next})
	}
	s.writeMu.Unlock()

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
	items := s.load().items

	out := identity.ActiveExemptions{
		Entries: make([]identity.ActiveExemption, 0, len(items)),
	}
	for _, e := range items {
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
	items := s.load().items

	result := make([]*Exemption, 0, len(items))
	for _, e := range items {
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
	// NO LOCK on this path. One atomic load of an immutable snapshot; see the Store
	// doc for why an RWMutex was wrong here even reduced to one acquisition.
	items := s.load().items

	// Fast path for the production-common state. Temporary exemptions are a
	// break-glass tool — granted rarely, expiring on their own — so most
	// deployments hold none, yet this runs on every bumped request. Walking an
	// empty set, and deriving the two per-request values to do it, is pure overhead.
	if len(items) == 0 {
		return false, nil
	}

	// Hoist the per-REQUEST work out of the per-ENTRY loop. matchSourceIP and
	// matchTargetHost each re-derived these from the same two arguments on
	// every iteration, so a store with N exemptions paid N redundant
	// net.ParseIP calls and N redundant strings.ToLower calls. Both results are
	// identical for every entry, so they are computed once here.
	clientIP := net.ParseIP(sourceIP)
	hostLower := strings.ToLower(targetHost)
	now := time.Now()

	for _, e := range items {
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
		if !matchSourceIPParsed(e.SourceIP, clientIP) {
			continue
		}
		if !matchTargetHostLowered(e.TargetHost, hostLower) {
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
	s.writeMu.Lock()
	cur := s.load().items
	next := make([]*Exemption, 0, len(cur))
	removed := 0
	for _, e := range cur {
		if e.ExpiresAt.Before(now) {
			removed++
			continue
		}
		next = append(next, e)
	}
	// Only swap when something actually expired — an unconditional store would
	// publish a new snapshot on every tick and churn the pointer for nothing.
	if removed > 0 {
		s.current.Store(&snapshot{items: next})
	}
	s.writeMu.Unlock()

	if removed > 0 {
		s.logger.Debug("expired exemptions purged", "count", removed)
	}
}
