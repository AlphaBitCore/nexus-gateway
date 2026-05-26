package passthrough

import (
	"sync/atomic"
	"time"
)

// TierEntry mirrors one DB row from the
// gateway_passthrough_config_{global,adapter,provider} tables. The JSON
// tags match the Hub-delivered shadow blob shape (matches the
// thing_config_template seed in migration
// 20260517000000_e48_gateway_passthrough_config_3tier).
type TierEntry struct {
	Enabled         bool       `json:"enabled"`
	BypassHooks     bool       `json:"bypassHooks"`
	BypassCache     bool       `json:"bypassCache"`
	BypassNormalize bool       `json:"bypassNormalize"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	EnabledBy       string     `json:"enabledBy,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

// active reports whether this tier should contribute to the effective
// config: requires Enabled=true AND expires_at in the future. A nil
// ExpiresAt while Enabled=true is treated as expired (defence-in-depth
// against a snapshot that bypassed the DB CHECK).
func (e TierEntry) active(now time.Time) bool {
	if !e.Enabled {
		return false
	}
	if e.ExpiresAt == nil {
		return false
	}
	return e.ExpiresAt.After(now)
}

// Snapshot is the 3-tier passthrough configuration as a single
// in-memory artefact. Adapters and Providers are nullable (a tier with
// zero rows omits its map entirely in the wire blob).
//
// Treat as read-only after the Cache exposes it via Effective. The
// Cache swaps the entire Snapshot atomically; in-flight readers keep
// referencing the pre-swap pointer.
type Snapshot struct {
	Global    TierEntry            `json:"global"`
	Adapters  map[string]TierEntry `json:"adapters,omitempty"`
	Providers map[string]TierEntry `json:"providers,omitempty"`
}

// Effective returns the merged *Config for the given primary target
// (its provider_id + adapter_type). Merge order:
//
//  1. global (most permissive)
//  2. adapter override
//  3. provider override (most specific)
//
// Each tier contributes if active(now). Bypass flags are OR'd across
// active tiers (any tier enabling a bypass turns it on); ExpiresAt is
// the soonest among active tiers (tightest bound wins). EnabledBy and
// Reason attribute to the most specific active tier so the audit log
// surfaces who actually triggered the bypass.
//
// Returns nil when no tier is active for this (provider, adapter)
// pair. Nil is the correct "no bypass" value downstream — both
// AnyBypassActive and Flags are nil-safe (S2).
func (s *Snapshot) Effective(providerID, adapterType string) *Config {
	if s == nil {
		return nil
	}
	now := time.Now()

	tiers := []TierEntry{}
	if s.Global.active(now) {
		tiers = append(tiers, s.Global)
	}
	if adapterType != "" {
		if t, ok := s.Adapters[adapterType]; ok && t.active(now) {
			tiers = append(tiers, t)
		}
	}
	if providerID != "" {
		if t, ok := s.Providers[providerID]; ok && t.active(now) {
			tiers = append(tiers, t)
		}
	}
	if len(tiers) == 0 {
		return nil
	}

	out := &Config{Enabled: true}
	var earliest *time.Time
	for _, t := range tiers {
		if t.BypassHooks {
			out.BypassHooks = true
		}
		if t.BypassCache {
			out.BypassCache = true
		}
		if t.BypassNormalize {
			out.BypassNormalize = true
		}
		// Tighter expiry wins.
		if t.ExpiresAt != nil && (earliest == nil || t.ExpiresAt.Before(*earliest)) {
			earliest = t.ExpiresAt
		}
		// Most specific wins for attribution; we iterate global -> adapter
		// -> provider, so the last write wins naturally.
		if t.EnabledBy != "" {
			out.EnabledBy = t.EnabledBy
		}
		if t.Reason != "" {
			out.Reason = t.Reason
		}
	}
	if earliest != nil {
		out.ExpiresAt = *earliest
	}
	return out
}

// Cache holds the live Snapshot behind an atomic.Pointer for lock-free
// per-request reads. Cold-start state is the empty Snapshot — every
// Effective lookup returns nil, matching the fail-closed invariant
// (requirements M4).
//
// Hub config-applier callbacks call SetSnapshot on every push; readers
// (the request handler at Phase 4.5) call Effective; the two never
// block each other.
type Cache struct {
	ptr atomic.Pointer[Snapshot]
}

// NewCache returns a Cache with an empty Snapshot installed. Safe for
// concurrent use. The empty snapshot makes the cold-start fail-closed
// contract a structural property: until Hub pushes the first real
// snapshot, every Effective returns nil and no bypass can fire.
func NewCache() *Cache {
	c := &Cache{}
	c.ptr.Store(&Snapshot{})
	return c
}

// SetSnapshot atomically replaces the live snapshot. Callers pass an
// already-parsed *Snapshot; the cache retains the pointer as-is and
// does not copy. In-flight readers retain the pre-swap pointer
// untouched.
//
// A nil snapshot is normalised to an empty Snapshot to preserve the
// cold-start contract (Effective always returns nil rather than
// panicking).
func (c *Cache) SetSnapshot(s *Snapshot) {
	if c == nil {
		return
	}
	if s == nil {
		s = &Snapshot{}
	}
	c.ptr.Store(s)
}

// Effective returns the merged *Config for the supplied primary target
// (its provider_id + adapter_type). Returns nil when no tier is
// active. Nil-safe.
func (c *Cache) Effective(providerID, adapterType string) *Config {
	if c == nil {
		return nil
	}
	snap := c.ptr.Load()
	return snap.Effective(providerID, adapterType)
}
