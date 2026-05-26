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

// TestConfigCache_Overwrite verifies that the most recent Set wins.
func TestConfigCache_Overwrite(t *testing.T) {
	c := NewConfigCache()
	c.Set(ConfigSnapshot{EmbeddingProviderID: "first"})
	c.Set(ConfigSnapshot{EmbeddingProviderID: "second"})
	if got := c.Get().EmbeddingProviderID; got != "second" {
		t.Errorf("expected 'second', got %q", got)
	}
}
