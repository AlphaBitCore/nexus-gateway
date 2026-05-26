package passthrough

import (
	"sync"
	"testing"
	"time"
)

func futureExpiry(d time.Duration) *time.Time {
	t := time.Now().Add(d)
	return &t
}

func pastExpiry(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}

func TestCache_NewCache_ColdStart_EffectiveReturnsNil(t *testing.T) {
	c := NewCache()
	if got := c.Effective("p-1", "anthropic"); got != nil {
		t.Errorf("cold-start cache: Effective() = %v, want nil", got)
	}
}

func TestCache_NilReceiver_Safe(t *testing.T) {
	var c *Cache
	if got := c.Effective("p-1", "anthropic"); got != nil {
		t.Errorf("nil cache: Effective() = %v, want nil", got)
	}
	c.SetSnapshot(&Snapshot{}) // must not panic
}

func TestSnapshot_GlobalOnly_AppliesToEveryProvider(t *testing.T) {
	s := &Snapshot{
		Global: TierEntry{
			Enabled: true, BypassHooks: true,
			ExpiresAt: futureExpiry(1 * time.Hour),
			Reason:    "global incident",
		},
	}
	cfg := s.Effective("p-anything", "openai")
	if cfg == nil || !cfg.AnyBypassActive() || !cfg.BypassHooks {
		t.Fatalf("global-only: expected BypassHooks active, got %#v", cfg)
	}
	if cfg.Reason != "global incident" {
		t.Errorf("Reason = %q, want global incident", cfg.Reason)
	}
}

func TestSnapshot_AdapterOnly_AppliesToMatchingProviders(t *testing.T) {
	s := &Snapshot{
		Adapters: map[string]TierEntry{
			"anthropic": {Enabled: true, BypassCache: true, ExpiresAt: futureExpiry(1 * time.Hour)},
		},
	}
	if cfg := s.Effective("p-1", "anthropic"); cfg == nil || !cfg.BypassCache {
		t.Errorf("anthropic provider should hit adapter-tier; got %#v", cfg)
	}
	if cfg := s.Effective("p-2", "openai"); cfg != nil {
		t.Errorf("openai provider should NOT hit anthropic adapter-tier; got %#v", cfg)
	}
}

func TestSnapshot_ProviderOnly_AppliesOnlyToSpecificProvider(t *testing.T) {
	s := &Snapshot{
		Providers: map[string]TierEntry{
			"p-target": {Enabled: true, BypassNormalize: true, BypassCache: true, ExpiresAt: futureExpiry(1 * time.Hour)},
		},
	}
	if cfg := s.Effective("p-target", "openai"); cfg == nil || !cfg.BypassNormalize || !cfg.BypassCache {
		t.Errorf("p-target should hit provider-tier with normalize+cache bypass; got %#v", cfg)
	}
	if cfg := s.Effective("p-other", "openai"); cfg != nil {
		t.Errorf("p-other should NOT hit p-target's provider-tier; got %#v", cfg)
	}
}

func TestSnapshot_3TierMerge_FlagsOrAcrossTiers(t *testing.T) {
	s := &Snapshot{
		Global: TierEntry{Enabled: true, BypassHooks: true, ExpiresAt: futureExpiry(3 * time.Hour)},
		Adapters: map[string]TierEntry{
			"anthropic": {Enabled: true, BypassCache: true, ExpiresAt: futureExpiry(2 * time.Hour)},
		},
		Providers: map[string]TierEntry{
			"p-1": {Enabled: true, BypassNormalize: true, BypassCache: true, ExpiresAt: futureExpiry(1 * time.Hour), Reason: "p1 specific"},
		},
	}
	cfg := s.Effective("p-1", "anthropic")
	if cfg == nil {
		t.Fatalf("3-tier merge: expected non-nil config")
	}
	if !cfg.BypassHooks || !cfg.BypassCache || !cfg.BypassNormalize {
		t.Errorf("all three bypass flags should OR across tiers; got %#v", cfg)
	}
	// Provider tier has the soonest expiry (1h vs 2h vs 3h) — wins.
	if cfg.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt should be non-zero, got zero")
	}
	// Most-specific tier wins for Reason.
	if cfg.Reason != "p1 specific" {
		t.Errorf("Reason = %q, want p1 specific (most specific tier wins)", cfg.Reason)
	}
}

func TestSnapshot_ExpiredTier_FilteredOutAtLookup(t *testing.T) {
	s := &Snapshot{
		Global: TierEntry{Enabled: true, BypassHooks: true, ExpiresAt: pastExpiry(1 * time.Hour)},
		Providers: map[string]TierEntry{
			"p-1": {Enabled: true, BypassNormalize: true, BypassCache: true, ExpiresAt: futureExpiry(30 * time.Minute)},
		},
	}
	cfg := s.Effective("p-1", "openai")
	if cfg == nil {
		t.Fatalf("provider tier active: expected non-nil config")
	}
	if cfg.BypassHooks {
		t.Errorf("expired global tier should NOT contribute BypassHooks; got %#v", cfg)
	}
	if !cfg.BypassNormalize || !cfg.BypassCache {
		t.Errorf("provider tier should contribute Normalize+Cache; got %#v", cfg)
	}
}

func TestSnapshot_EnabledWithoutExpiresAt_TreatedAsExpired(t *testing.T) {
	// Defence-in-depth: DB CHECK enforces expires_at NOT NULL when
	// enabled, but if a snapshot bypasses the DB the in-memory layer
	// should not silently grant the bypass.
	s := &Snapshot{Global: TierEntry{Enabled: true, BypassHooks: true, ExpiresAt: nil}}
	if cfg := s.Effective("p", "openai"); cfg != nil {
		t.Errorf("Enabled+nil ExpiresAt should not be active; got %#v", cfg)
	}
}

func TestCache_SetSnapshot_AtomicSwapVisibleToReader(t *testing.T) {
	c := NewCache()
	if got := c.Effective("p", "openai"); got != nil {
		t.Fatalf("initial cold cache: expected nil, got %v", got)
	}
	c.SetSnapshot(&Snapshot{
		Global: TierEntry{Enabled: true, BypassHooks: true, ExpiresAt: futureExpiry(1 * time.Hour)},
	})
	got := c.Effective("p", "openai")
	if got == nil || !got.BypassHooks {
		t.Errorf("after SetSnapshot: expected BypassHooks active, got %v", got)
	}
}

func TestCache_SetSnapshot_NilReplacedWithEmpty(t *testing.T) {
	c := NewCache()
	c.SetSnapshot(&Snapshot{
		Global: TierEntry{Enabled: true, BypassHooks: true, ExpiresAt: futureExpiry(1 * time.Hour)},
	})
	c.SetSnapshot(nil) // should normalise to empty snapshot, not panic
	if got := c.Effective("p", "openai"); got != nil {
		t.Errorf("after SetSnapshot(nil): expected nil, got %v", got)
	}
}

func TestCache_ConcurrentSetAndEffective_NoRace(t *testing.T) {
	c := NewCache()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 100 {
				c.SetSnapshot(&Snapshot{
					Global: TierEntry{Enabled: true, BypassHooks: true, ExpiresAt: futureExpiry(time.Hour)},
				})
			}
		}(i)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = c.Effective("p", "openai")
			}
		}()
	}
	wg.Wait()
}
