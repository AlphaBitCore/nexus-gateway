// proxy_l2_trylookup_test.go — tryL2Lookup branch matrix, relocated out of
// proxy_l2_test.go (split-on-touch policy: that file crossed 800 lines while
// this test group was being extended). Shares fixtures defined in
// proxy_l2_test.go (stubSemanticReader, makeTryParams, enabledFleetCache,
// stubCredManager, noopLogger) — same package, no import needed for those.
package proxy

import (
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// tryL2Lookup — branch matrix with fleet-gated policy

func TestTryL2Lookup_NilReader(t *testing.T) {
	h := &Handler{deps: &Deps{SemanticConfigCache: enabledFleetCache()}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false when SemanticReader nil")
	}
}

// TestTryL2Lookup_NilCredManager covers the defensive guard at the read
// path's credential lookup: SemanticReader is wired and the fleet policy
// resolves, but CredManager is absent (boot-time wiring would always
// supply one; this guards hand-constructed Handler test doubles). Must
// return false without invoking the reader.
func TestTryL2Lookup_NilCredManager(t *testing.T) {
	rdr := &stubSemanticReader{}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache()}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false when CredManager nil")
	}
	if rdr.called.Load() != 0 {
		t.Error("reader must not be called when CredManager nil")
	}
}

func TestTryL2Lookup_FleetDisabled(t *testing.T) {
	rdr := &stubSemanticReader{}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: semantic.NewConfigCache()}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false when fleet config disabled")
	}
	if rdr.called.Load() != 0 {
		t.Error("reader should not be called when fleet disabled")
	}
}

func TestTryL2Lookup_NoCanonicalMsgs(t *testing.T) {
	rdr := &stubSemanticReader{}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	p.canonicalMsgs = nil
	if h.tryL2Lookup(p) {
		t.Error("want false when no canonical messages")
	}
	if rdr.called.Load() != 0 {
		t.Error("reader should not be called on empty embedding input")
	}
	// no canonical messages → no text to embed, which must stamp
	// the accurate no_embeddable_text reason, NOT the oversize reason (the
	// input was never oversize — there was simply nothing to embed).
	if p.rec.GatewayCacheSkipReason != audit.GatewayCacheSkipReasonNoEmbeddableText {
		t.Errorf("skip reason: got %q, want %q",
			p.rec.GatewayCacheSkipReason, audit.GatewayCacheSkipReasonNoEmbeddableText)
	}
}

func TestTryL2Lookup_ReaderError(t *testing.T) {
	rdr := &stubSemanticReader{err: errors.New("read failed")}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false on reader error")
	}
}

func TestTryL2Lookup_ReaderMiss(t *testing.T) {
	rdr := &stubSemanticReader{result: semantic.ReadResult{Outcome: "miss"}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false on reader miss")
	}
}

// TestTryL2Lookup_StampsEmbeddingProviderAlongsideCost pins the fix for the
// L2 embedding-cost attribution gap: tryL2Lookup must stamp
// rec.EmbeddingProviderID from the Reader's result (itself sourced from
// ConfigSnapshot.EmbeddingProviderID — the exact provider id passed to the
// embedding call, never inferred from a model id) alongside
// rec.EmbeddingCostUsd, so the L2 embedding spend can be attributed to a
// provider dimension downstream.
func TestTryL2Lookup_StampsEmbeddingProviderAlongsideCost(t *testing.T) {
	rdr := &stubSemanticReader{result: semantic.ReadResult{
		Outcome:             "miss",
		EmbeddingCostUSD:    0.000004,
		EmbeddingProviderID: "prov-openai",
	}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	if h.tryL2Lookup(p) {
		t.Error("want false on reader miss")
	}
	if p.rec.EmbeddingProviderID != "prov-openai" {
		t.Errorf("EmbeddingProviderID = %q, want prov-openai", p.rec.EmbeddingProviderID)
	}
	if p.rec.EmbeddingCostUsd != 0.000004 {
		t.Errorf("EmbeddingCostUsd = %v, want 0.000004", p.rec.EmbeddingCostUsd)
	}
}

// TestTryL2Lookup_ReaderSkip_StampsReason also covers the case where the
// Reader never issued an embedding call (skip_disabled reached before
// sf.Embed): EmbeddingProviderID must stay empty since no call means nothing
// to attribute.
func TestTryL2Lookup_ReaderSkip_LeavesEmbeddingProviderEmpty(t *testing.T) {
	rdr := &stubSemanticReader{result: semantic.ReadResult{
		SkipReason: audit.GatewayCacheSkipReasonEmbeddingTimeout,
	}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	if h.tryL2Lookup(p) {
		t.Error("want false on reader skip")
	}
	if p.rec.EmbeddingProviderID != "" {
		t.Errorf("EmbeddingProviderID = %q, want empty when no embedding call was issued", p.rec.EmbeddingProviderID)
	}
}

func TestTryL2Lookup_ReaderSkip_StampsReason(t *testing.T) {
	rdr := &stubSemanticReader{result: semantic.ReadResult{
		SkipReason: audit.GatewayCacheSkipReasonEmbeddingTimeout,
	}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	if h.tryL2Lookup(p) {
		t.Error("want false on reader skip")
	}
	if p.rec.GatewayCacheSkipReason != audit.GatewayCacheSkipReasonEmbeddingTimeout {
		t.Errorf("skip reason not propagated: got %q", p.rec.GatewayCacheSkipReason)
	}
}

func TestTryL2Lookup_HitStream_ConversionError(t *testing.T) {
	// Stream HIT with malformed chunk array → ToCacheStreamEntry returns error
	// → handler resets stamps and returns false so broker dispatch can retry.
	rdr := &stubSemanticReader{result: semantic.ReadResult{
		Entry: &semantic.Entry{
			ResponseBody: []byte(`{not a valid chunk array}`),
			EntryKey:     "nexus:semantic-cache:v1:1234567890abcdef",
		},
	}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	p.isStream = true
	if h.tryL2Lookup(p) {
		t.Error("want false on stream conversion error")
	}
	if p.rec.GatewayCacheStatus != "" {
		t.Errorf("status should be reset; got %q", p.rec.GatewayCacheStatus)
	}
	if p.rec.GatewayCacheKind != "" {
		t.Errorf("kind should be reset; got %q", p.rec.GatewayCacheKind)
	}
	// GatewayCacheL2EntryKey must also be reset on the stream-conversion
	// fallback path so the broker re-stamps it cleanly (otherwise the
	// failing partial stamp would leak into the audit row).
	if p.rec.GatewayCacheL2EntryKey != "" {
		t.Errorf("L2 entry key should be reset; got %q", p.rec.GatewayCacheL2EntryKey)
	}
}
