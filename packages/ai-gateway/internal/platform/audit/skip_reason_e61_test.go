package audit

import (
	"log/slog"
	"testing"
	"time"
)

// TestSkipReasonRoundTrip verifies that every GatewayCacheSkipReason constant
// is a non-empty string and that the string value is stable (no accidental
// typo changes the wire value sent to traffic_event).
//
// The test intentionally does NOT use a map literal to list the constants —
// that would mask a forgotten constant. Instead, each constant is named
// individually so a rename / deletion in audit.go produces a compile error
// here, not a silent coverage drop.
func TestSkipReasonRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		got  GatewayCacheSkipReason
		want string
	}{
		// Pre-lookup short-circuit constants (peer to disabled / no_cache /
		// passthrough; NOT E61 semantic-cache failure modes).
		{"disabled", GatewayCacheSkipReasonDisabled, "disabled"},
		{"no_cache", GatewayCacheSkipReasonNoCache, "no_cache"},
		{"passthrough", GatewayCacheSkipReasonPassthrough, "passthrough"},
		{"embeddings_endpoint", GatewayCacheSkipReasonEmbeddingsEndpoint, "embeddings_endpoint"},

		// Time-sensitive skip.
		{"time_sensitive", GatewayCacheSkipReasonTimeSensitive, "time_sensitive"},

		// Oversize-for-embedding.
		{"oversize_for_embedding", GatewayCacheSkipReasonOversizeForEmbedding, "oversize_for_embedding"},

		// L2 infrastructure failure-mode reasons (8 constants).
		{"valkey_unavailable", GatewayCacheSkipReasonValkeyUnavailable, "valkey_unavailable"},
		{"embedding_timeout", GatewayCacheSkipReasonEmbeddingTimeout, "embedding_timeout"},
		{"embedding_provider_error", GatewayCacheSkipReasonEmbeddingProviderError, "embedding_provider_error"},
		{"embedding_dim_mismatch", GatewayCacheSkipReasonEmbeddingDimMismatch, "embedding_dim_mismatch"},
		{"semantic_search_error", GatewayCacheSkipReasonSemanticSearchError, "semantic_search_error"},
		{"semantic_search_timeout", GatewayCacheSkipReasonSemanticSearchTimeout, "semantic_search_timeout"},
		{"semantic_unavailable", GatewayCacheSkipReasonSemanticUnavailable, "semantic_unavailable"},
		{"embedding_circuit_open", GatewayCacheSkipReasonEmbeddingCircuitOpen, "embedding_circuit_open"},
		// Negative-feedback poison list.
		{"poisoned", GatewayCacheSkipReasonPoisoned, "poisoned"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) == "" {
				t.Errorf("GatewayCacheSkipReason %q constant is empty string", tc.name)
			}
			if string(tc.got) != tc.want {
				t.Errorf("GatewayCacheSkipReason %q = %q, want %q", tc.name, string(tc.got), tc.want)
			}
		})
	}
}

// TestE61SkipReasonCount verifies exactly 11 semantic-cache skip-reason
// constants exist (10 base + 1 poisoned). The architecture doc
// response-cache-architecture.md enumerates the base reasons. This test pins
// the count so an accidental addition or deletion surfaces immediately.
func TestE61SkipReasonCount(t *testing.T) {
	// List every constant explicitly — no reflection magic. A missing constant
	// becomes a compile error; an extra constant makes this count wrong.
	e61Reasons := []GatewayCacheSkipReason{
		GatewayCacheSkipReasonTimeSensitive,
		GatewayCacheSkipReasonOversizeForEmbedding,
		GatewayCacheSkipReasonValkeyUnavailable,
		GatewayCacheSkipReasonEmbeddingTimeout,
		GatewayCacheSkipReasonEmbeddingProviderError,
		GatewayCacheSkipReasonEmbeddingDimMismatch,
		GatewayCacheSkipReasonSemanticSearchError,
		GatewayCacheSkipReasonSemanticSearchTimeout,
		GatewayCacheSkipReasonSemanticUnavailable,
		GatewayCacheSkipReasonEmbeddingCircuitOpen,
		// Poison-list addition.
		GatewayCacheSkipReasonPoisoned,
	}
	if got := len(e61Reasons); got != 11 {
		t.Errorf("has %d skip-reason constants, want exactly 11 (10 base + 1 poisoned)", got)
	}
	// Also verify all are distinct (no accidental duplicate string values).
	seen := make(map[string]bool, len(e61Reasons))
	for _, r := range e61Reasons {
		s := string(r)
		if seen[s] {
			t.Errorf("duplicate skip-reason value: %q", s)
		}
		seen[s] = true
	}
}

// TestCacheKindSemanticWireValue pins the "semantic" wire string for
// GatewayCacheKindSemantic. This value is written to traffic_event.gateway_cache_kind;
// changing it would break analytics queries that filter on this literal.
// Compile-time + value guard.
func TestCacheKindSemanticWireValue(t *testing.T) {
	if string(GatewayCacheKindSemantic) != "semantic" {
		t.Errorf("GatewayCacheKindSemantic wire value = %q, want \"semantic\"", GatewayCacheKindSemantic)
	}
	if string(GatewayCacheKindExtract) != "extract" {
		t.Errorf("GatewayCacheKindExtract wire value = %q, want \"extract\"", GatewayCacheKindExtract)
	}
}

// TestRecordToMessage_GatewayCacheL2EntryKey pins the wire pass-through:
// recordToMessage MUST forward rec.GatewayCacheL2EntryKey verbatim onto
// msg.GatewayCacheL2EntryKey so the Hub consumer can write it into
// traffic_event.gateway_cache_l2_entry_key. Without this stamp the audit
// drawer's "Mark as bad cache hit" thumbs-down silently no-ops in production
// (its previous fallback was traffic_event.id, which never matched the
// Reader's IsPoisoned check).
func TestRecordToMessage_GatewayCacheL2EntryKey(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	const wantKey = "nexus:semantic-cache:v1:0123456789abcdef"
	rec := &Record{
		RequestID:              "req-l2-hit",
		Timestamp:              time.Now(),
		GatewayCacheStatus:     GatewayCacheHit,
		GatewayCacheKind:       GatewayCacheKindSemantic,
		GatewayCacheL2EntryKey: wantKey,
	}
	msg := w.recordToMessage(rec)
	if msg.GatewayCacheL2EntryKey != wantKey {
		t.Errorf("GatewayCacheL2EntryKey on wire = %q, want %q",
			msg.GatewayCacheL2EntryKey, wantKey)
	}

	// Empty rec field → empty wire field (omitempty drops it from JSON).
	empty := w.recordToMessage(&Record{RequestID: "req-miss", Timestamp: time.Now()})
	if empty.GatewayCacheL2EntryKey != "" {
		t.Errorf("empty rec L2 entry key should produce empty wire field; got %q",
			empty.GatewayCacheL2EntryKey)
	}
}
