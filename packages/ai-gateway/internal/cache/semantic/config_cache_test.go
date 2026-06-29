package semantic

import (
	"sync"
	"testing"
	"time"
)

// TestConfigCache_ZeroValue verifies that Get on a freshly-constructed cache
// returns a zero-valued snapshot (never panics) and EffectiveEnabled is false.
func TestConfigCache_ZeroValue(t *testing.T) {
	c := NewConfigCache()
	snap := c.Get()
	if snap.Enabled {
		t.Fatal("zero snapshot should not be enabled")
	}
	if snap.Fingerprint != "" {
		t.Fatal("zero snapshot should have empty fingerprint")
	}
	if c.EffectiveEnabled() {
		t.Fatal("EffectiveEnabled should be false on zero cache")
	}
}

// TestConfigCache_SetGet verifies that Set + Get round-trips all fields.
func TestConfigCache_SetGet(t *testing.T) {
	c := NewConfigCache()
	now := time.Now().Truncate(time.Second)
	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "text-embedding-3-small",
		EmbeddingDimension:  1536,
		Fingerprint:         "abc123",
		RedisIndexName:      "nexus:semantic-cache:v1",
		UpdatedAt:           now,
	}
	c.Set(snap)
	got := c.Get()
	if got.EmbeddingProviderID != snap.EmbeddingProviderID {
		t.Errorf("ProviderID: got %q, want %q", got.EmbeddingProviderID, snap.EmbeddingProviderID)
	}
	if got.Fingerprint != snap.Fingerprint {
		t.Errorf("Fingerprint: got %q, want %q", got.Fingerprint, snap.Fingerprint)
	}
	if got.EmbeddingDimension != snap.EmbeddingDimension {
		t.Errorf("Dimension: got %d, want %d", got.EmbeddingDimension, snap.EmbeddingDimension)
	}
}

// TestConfigCache_EffectiveEnabled exercises the four conditions.
func TestConfigCache_EffectiveEnabled(t *testing.T) {
	cases := []struct {
		name    string
		snap    ConfigSnapshot
		wantYes bool
	}{
		{
			name:    "all conditions met",
			snap:    ConfigSnapshot{Enabled: true, EmbeddingProviderID: "p", EmbeddingModelID: "m", EmbeddingDimension: 1},
			wantYes: true,
		},
		{
			name:    "disabled kill switch",
			snap:    ConfigSnapshot{Enabled: false, EmbeddingProviderID: "p", EmbeddingModelID: "m", EmbeddingDimension: 1},
			wantYes: false,
		},
		{
			name:    "missing provider",
			snap:    ConfigSnapshot{Enabled: true, EmbeddingProviderID: "", EmbeddingModelID: "m", EmbeddingDimension: 1},
			wantYes: false,
		},
		{
			name:    "missing model",
			snap:    ConfigSnapshot{Enabled: true, EmbeddingProviderID: "p", EmbeddingModelID: "", EmbeddingDimension: 1},
			wantYes: false,
		},
		{
			name:    "zero dimension",
			snap:    ConfigSnapshot{Enabled: true, EmbeddingProviderID: "p", EmbeddingModelID: "m", EmbeddingDimension: 0},
			wantYes: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConfigCache()
			c.Set(tc.snap)
			if got := c.EffectiveEnabled(); got != tc.wantYes {
				t.Errorf("EffectiveEnabled() = %v, want %v", got, tc.wantYes)
			}
		})
	}
}

// TestConfigCache_Concurrency verifies that concurrent Set + Get do not race.
func TestConfigCache_Concurrency(t *testing.T) {
	c := NewConfigCache()
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			c.Set(ConfigSnapshot{
				Enabled:             true,
				EmbeddingProviderID: "p",
				EmbeddingModelID:    "m",
				EmbeddingDimension:  i + 1,
				Fingerprint:         "fp",
				RedisIndexName:      "idx",
			})
		}(i)
		go func() {
			defer wg.Done()
			_ = c.Get()
		}()
	}
	wg.Wait()
}

// TestConfigCache_ScopeReady verifies the boot-window fail-closed signal the
// L1 exact-match tier gates on: false until the first Set, true afterwards, and
// true on a nil receiver (no semantic ConfigCache wired = documented fleet-wide,
// not a transient window).
func TestConfigCache_ScopeReady(t *testing.T) {
	var nilCache *ConfigCache
	if !nilCache.ScopeReady() {
		t.Fatal("nil ConfigCache should report ScopeReady=true (fleet-wide default)")
	}

	c := NewConfigCache()
	if c.ScopeReady() {
		t.Fatal("ScopeReady must be false before the first Set (boot window: VaryBy unknown)")
	}
	// VaryBy is the value the L1 tier needs; before Set it is the zero "".
	if c.Get().VaryBy != "" {
		t.Fatalf("pre-Set VaryBy = %q, want empty", c.Get().VaryBy)
	}
	c.Set(ConfigSnapshot{Enabled: false}) // even a disabled push flips loaded
	if !c.ScopeReady() {
		t.Fatal("ScopeReady must be true after the first Set, even a disabled snapshot")
	}
	// Set normalises an empty VaryBy to the strict "vk" default, so the L1 key
	// now isolates rather than running fleet-wide.
	if got := c.Get().VaryBy; got != "vk" {
		t.Fatalf("post-Set default VaryBy = %q, want \"vk\"", got)
	}
}

// TestConfigCache_Overwrite verifies that the most recent Set wins.
func TestConfigCache_Overwrite(t *testing.T) {
	c := NewConfigCache()
	c.Set(ConfigSnapshot{EmbeddingProviderID: "first"})
	c.Set(ConfigSnapshot{EmbeddingProviderID: "second"})
	if got := c.Get().EmbeddingProviderID; got != "second" {
		t.Errorf("expected 'second', got %q", got)
	}
}
