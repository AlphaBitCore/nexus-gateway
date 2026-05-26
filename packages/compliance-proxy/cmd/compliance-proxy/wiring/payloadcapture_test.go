package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
)

func TestInitPayloadCaptureStore_NilDB_ReturnsDefaultStore(t *testing.T) {
	store := InitPayloadCaptureStore(nil, nil, testLogger())
	if store == nil {
		t.Fatal("expected non-nil payload capture store")
	}
	cfg := store.Get()
	// Default config should be valid (non-zero threshold).
	if cfg.MaxInlineBodyBytes <= 0 {
		t.Errorf("default MaxInlineBodyBytes = %d; want > 0", cfg.MaxInlineBodyBytes)
	}
}

func TestInitPayloadCaptureStore_NilEmitterIsNoop(t *testing.T) {
	// emitter=nil must not panic.
	store := InitPayloadCaptureStore(nil, nil, testLogger())
	if store == nil {
		t.Fatal("expected non-nil store")
	}
}

// TestInitPayloadCaptureStore_NonNilEmitter_WiresStore exercises lines 33-35
// (emitter.WithPayloadCaptureStore) by passing a real AuditEmitter.
func TestInitPayloadCaptureStore_NonNilEmitter_WiresStore(t *testing.T) {
	writer := &stubAuditWriter{}
	emitter := compliance.NewAuditEmitter(writer, testLogger())
	store := InitPayloadCaptureStore(nil, emitter, testLogger())
	if store == nil {
		t.Fatal("expected non-nil store even with non-nil emitter")
	}
}

func TestInitStreamingPolicyStore_NilDBReturnsDefault(t *testing.T) {
	store := InitStreamingPolicyStore(nil, testLogger())
	if store == nil {
		t.Fatal("InitStreamingPolicyStore returned nil Store")
	}
	// When DB is nil we expect the Store seeded with DefaultPolicy
	// (non-empty Mode). #115: the shared streampolicy.BootStore
	// helper guarantees this invariant across all 3 services.
	policy := store.Get()
	if string(policy.Mode) == "" {
		t.Errorf("expected non-empty policy mode from nil-DB path, got %q", policy.Mode)
	}
}
