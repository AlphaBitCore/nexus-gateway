package wiring

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// stubMetadataReader is a test double for MetadataReader.
type stubMetadataReader struct {
	data map[string]json.RawMessage
	err  error
}

func (s *stubMetadataReader) GetSystemMetadata(_ context.Context, key string) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.data[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewReliabilityConfig_defaultsLoaded verifies that NewReliabilityConfig
// seeds the global snapshot with DefaultThresholds.
func TestNewReliabilityConfig_defaultsLoaded(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	snap := rc.Snapshot()
	def := credstate.DefaultThresholds
	if snap.AuthFailThreshold != def.AuthFailThreshold {
		t.Errorf("AuthFailThreshold: want %d got %d", def.AuthFailThreshold, snap.AuthFailThreshold)
	}
}

// TestReliabilityConfig_Reload_nilReader returns error.
func TestReliabilityConfig_Reload_nilReader(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	err := rc.Reload(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil reader")
	}
}

// TestReliabilityConfig_Reload_noRow keeps defaults when no row exists.
func TestReliabilityConfig_Reload_noRow(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	reader := &stubMetadataReader{data: map[string]json.RawMessage{}}
	if err := rc.Reload(context.Background(), reader); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Snapshot should still be defaults.
	snap := rc.Snapshot()
	if snap.AuthFailThreshold != credstate.DefaultThresholds.AuthFailThreshold {
		t.Error("expected defaults after no-row reload")
	}
}

// TestReliabilityConfig_Reload_readerError propagates DB error.
func TestReliabilityConfig_Reload_readerError(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	reader := &stubMetadataReader{err: errors.New("db error")}
	err := rc.Reload(context.Background(), reader)
	if err == nil {
		t.Fatal("expected error when reader returns error")
	}
}

// TestReliabilityConfig_Reload_malformedJSON returns parse error.
func TestReliabilityConfig_Reload_malformedJSON(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	reader := &stubMetadataReader{data: map[string]json.RawMessage{
		ReliabilityConfigKey: json.RawMessage(`{bad json`),
	}}
	err := rc.Reload(context.Background(), reader)
	if err == nil {
		t.Fatal("expected parse error for malformed JSON")
	}
}

// TestReliabilityConfig_Reload_invalidThresholds returns validation error.
func TestReliabilityConfig_Reload_invalidThresholds(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	// AuthFailThreshold < 0 is invalid per credstate.Thresholds.Validate.
	invalid := credstate.Thresholds{AuthFailThreshold: -1}
	raw, _ := json.Marshal(invalid)
	reader := &stubMetadataReader{data: map[string]json.RawMessage{
		ReliabilityConfigKey: raw,
	}}
	err := rc.Reload(context.Background(), reader)
	if err == nil {
		t.Fatal("expected validation error for invalid thresholds")
	}
}

// TestReliabilityConfig_Reload_validThresholds updates snapshot atomically.
func TestReliabilityConfig_Reload_validThresholds(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	newT := credstate.Thresholds{
		AuthFailThreshold:              5,
		RateLimitCooldownSeconds:       120,
		HealthyThresholdPct:            90,
		DegradedThresholdPct:           40,
		HealthMinSamples:               10,
		HealthWindowSeconds:            300,
		HealthSustainedDegradedSeconds: 600,
	}
	raw, _ := json.Marshal(newT)
	reader := &stubMetadataReader{data: map[string]json.RawMessage{
		ReliabilityConfigKey: raw,
	}}
	if err := rc.Reload(context.Background(), reader); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	snap := rc.Snapshot()
	if snap.AuthFailThreshold != 5 {
		t.Errorf("expected AuthFailThreshold=5, got %d", snap.AuthFailThreshold)
	}
	if snap.HealthyThresholdPct != 90 {
		t.Errorf("expected HealthyThresholdPct=90, got %d", snap.HealthyThresholdPct)
	}
}

// TestReliabilityConfig_Resolve_nilCache returns global when cache is nil.
func TestReliabilityConfig_Resolve_nilCache(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	result := rc.Resolve("some-credential-id")
	if result.AuthFailThreshold != credstate.DefaultThresholds.AuthFailThreshold {
		t.Errorf("expected default thresholds, got %+v", result)
	}
}

// TestReliabilityConfig_Resolve_emptyCredentialID returns global immediately.
func TestReliabilityConfig_Resolve_emptyCredentialID(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	result := rc.Resolve("")
	if result.AuthFailThreshold != credstate.DefaultThresholds.AuthFailThreshold {
		t.Errorf("expected default thresholds for empty credentialID, got %+v", result)
	}
}

// TestReliabilityConfig_Snapshot_returnsCurrentGlobal verifies Snapshot returns
// current atomic value.
func TestReliabilityConfig_Snapshot_returnsCurrentGlobal(t *testing.T) {
	rc := NewReliabilityConfig(nil, discardLogger())
	first := rc.Snapshot()

	// Reload with a value that is guaranteed to differ from any plausible
	// default so the assertion below is meaningful.
	newT := credstate.Thresholds{
		AuthFailThreshold:              7,
		HealthyThresholdPct:            85,
		DegradedThresholdPct:           30,
		HealthMinSamples:               3,
		HealthWindowSeconds:            120,
		RateLimitCooldownSeconds:       30,
		HealthSustainedDegradedSeconds: 300,
	}
	raw, _ := json.Marshal(newT)
	reader := &stubMetadataReader{data: map[string]json.RawMessage{
		ReliabilityConfigKey: raw,
	}}
	_ = rc.Reload(context.Background(), reader)
	second := rc.Snapshot()

	if second.AuthFailThreshold != 7 {
		t.Errorf("expected AuthFailThreshold=7 after reload, got %d", second.AuthFailThreshold)
	}
	if first.HealthyThresholdPct == second.HealthyThresholdPct &&
		first.DegradedThresholdPct == second.DegradedThresholdPct &&
		first.AuthFailThreshold == second.AuthFailThreshold {
		t.Errorf("snapshot should have changed after reload; first=%+v second=%+v", first, second)
	}
}

// TestReliabilityConfig_Reload_withLoggerNil verifies nil logger does not panic.
func TestReliabilityConfig_Reload_withLoggerNil(t *testing.T) {
	rc := NewReliabilityConfig(nil, nil)
	newT := credstate.Thresholds{
		AuthFailThreshold:              3,
		HealthyThresholdPct:            95,
		DegradedThresholdPct:           50,
		HealthMinSamples:               5,
		HealthWindowSeconds:            300,
		RateLimitCooldownSeconds:       60,
		HealthSustainedDegradedSeconds: 900,
	}
	raw, _ := json.Marshal(newT)
	reader := &stubMetadataReader{data: map[string]json.RawMessage{
		ReliabilityConfigKey: raw,
	}}
	// Should not panic even with nil logger.
	if err := rc.Reload(context.Background(), reader); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
