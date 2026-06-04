package semantic

import (
	"sync/atomic"
	"time"
)

// ConfigSnapshot is the in-process copy of the semantic_cache_config
// singleton row. Populated by the Hub shadow callback in ai-gateway main
// (wired in S4; this package only exposes the Set/Get contract).
type ConfigSnapshot struct {
	Enabled             bool
	EmbeddingProviderID string
	EmbeddingModelID    string

	// EmbeddingProviderBaseURL is the embedding upstream's HTTP base URL
	// ("https://api.openai.com") and EmbeddingProviderModelID is the
	// wire-facing model code ("text-embedding-3-small"). Both come from
	// the same Hub-pushed shadow blob as the other fields here — the CP
	// joins Provider + Model when it serialises the row so Reader/Writer
	// don't have to look them up per-call on the request hot path.
	EmbeddingProviderBaseURL string
	EmbeddingProviderModelID string
	// EmbeddingInputPricePerMillion is the embedding model's input-side
	// cost (USD per million tokens), joined from Model.inputPricePerMillion
	// at CP push time. Reader.Read divides by 1e6 to get per-token cost
	// and multiplies by the embedding response's promptTokens to stamp
	// rec.EmbeddingCostUsd. Snapshot-resident so no per-call DB lookup.
	EmbeddingInputPricePerMillion float64
	EmbeddingDimension            int
	// EmbeddingMaxInputTokens is the embedding model's context window (from the
	// model's capabilityJson.embeddings.max_input_tokens, joined at CP push
	// time). The L2 input shaper truncates the embed text to this many tokens
	// so a large chat context never exceeds the embedding model's limit. 0 →
	// the shaper falls back to a conservative default.
	EmbeddingMaxInputTokens int
	Fingerprint             string // sha256(provider:model:dim) — drives blue/green index lifecycle
	RedisIndexName          string // versioned, e.g. "nexus:semantic-cache:v1"
	// Threshold is the fleet-wide cosine similarity gate for L2 hits.
	// 0 → ConfigCache.Set fills the schema default (0.96) so a stale
	// snapshot from a pre-migration Hub never produces threshold=0.
	Threshold float32
	// VaryBy is the L2 cache isolation scope. Enum: none | user | vk | org.
	// Empty → ConfigCache.Set fills the schema default ("vk").
	VaryBy string
	// EmbedStrategy is the input shaping for L2 embedding. Enum: last_user |
	// system_plus_last_user | recent_turns | head_plus_tail | full_truncated.
	// Empty → ConfigCache.Set fills the schema default ("system_plus_last_user").
	EmbedStrategy string
	// AllowCrossModel lets an L2 lookup return an entry cached against a
	// different upstream model. Zero value is the safe default (false).
	AllowCrossModel bool
	UpdatedAt       time.Time
}

// ConfigCache is the thread-safe in-process snapshot of ConfigSnapshot.
// The hot path calls Get() — a single atomic load with no allocation.
// The Hub shadow callback calls Set() to push a fresh snapshot.
type ConfigCache struct {
	snap atomic.Pointer[ConfigSnapshot]
}

// NewConfigCache returns an empty ConfigCache. Get() returns a
// zero-valued snapshot until Set is called; EffectiveEnabled() returns
// false in that state.
func NewConfigCache() *ConfigCache {
	return &ConfigCache{}
}

// Set atomically replaces the current snapshot. Safe to call from any
// goroutine; callers must not mutate snap after calling Set.
//
// Fills in fleet-tuning defaults when the inbound snap leaves them at the
// Go zero value (e.g., a Hub push from a pre-migration row, or a unit test
// constructing a partial snapshot). This keeps the L2 hot path free of
// per-call default fallbacks.
func (c *ConfigCache) Set(snap ConfigSnapshot) {
	if snap.Threshold <= 0 || snap.Threshold > 1 {
		snap.Threshold = 0.96
	}
	switch snap.VaryBy {
	case "none", "user", "vk", "org":
		// allowed
	default:
		snap.VaryBy = "vk"
	}
	switch snap.EmbedStrategy {
	case "last_user", "system_plus_last_user", "recent_turns", "head_plus_tail", "full_truncated":
		// allowed
	default:
		snap.EmbedStrategy = "system_plus_last_user"
	}
	c.snap.Store(&snap)
}

// Get returns the current snapshot. Returns a zero-valued ConfigSnapshot
// when Set has never been called (gateway startup before the first Hub
// push).
func (c *ConfigCache) Get() ConfigSnapshot {
	if p := c.snap.Load(); p != nil {
		return *p
	}
	return ConfigSnapshot{}
}

// EffectiveEnabled returns true iff the L2 semantic cache is fully
// configured and not killed:
//
//   - Enabled == true (fleet-wide kill switch is not active)
//   - EmbeddingProviderID is non-empty
//   - EmbeddingModelID is non-empty
//   - EmbeddingDimension > 0
//
// A snapshot that passes this check but has an empty RedisIndexName is
// treated as misconfigured; EnsureIndex will fail cleanly and the
// IndexLifecycle will log the error. The Write path checks EffectiveEnabled
// first.
func (c *ConfigCache) EffectiveEnabled() bool {
	snap := c.Get()
	return snap.Enabled &&
		snap.EmbeddingProviderID != "" &&
		snap.EmbeddingModelID != "" &&
		snap.EmbeddingDimension > 0
}
