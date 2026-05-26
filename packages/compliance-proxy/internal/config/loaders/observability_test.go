package loaders

import (
	"database/sql"
	"errors"
	"testing"
)

// The DB-bound LoadObservabilityConfig is a thin shell that delegates to
// decodeObservabilityResult; the interesting decision tree (missing row,
// transient err, malformed JSON, success) is unit-tested here without a
// live database.

// TestDecodeObservabilityResult_MissingRowReturnsServiceNameOnlyDefault
// exercises the sql.ErrNoRows branch — a fresh deploy with no
// system_metadata["observability.config"] row must return a Config that
// stamps the service name so traces can still be filtered by service,
// while leaving Enabled false (no traces emitted until the admin opts in)
// and SamplingRate 0.
func TestDecodeObservabilityResult_MissingRowReturnsServiceNameOnlyDefault(t *testing.T) {
	got, err := decodeObservabilityResult(nil, sql.ErrNoRows)
	if err != nil {
		t.Fatalf("ErrNoRows must not propagate; got: %v", err)
	}
	if got == nil {
		t.Fatal("missing-row result must be non-nil so callers can dereference unconditionally")
	}
	if got.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("service name not stamped: %q", got.ServiceName)
	}
	if got.Enabled {
		t.Errorf("default must keep telemetry disabled until the admin enables it")
	}
	if got.SamplingRate != 0 {
		t.Errorf("default SamplingRate must be 0, got %v", got.SamplingRate)
	}
}

// TestDecodeObservabilityResult_GenericQueryErrorPropagates covers the
// transient-DB-error branch — a planner / conn-refused / timeout must
// surface to the caller (not silently default) so an outage is visible
// in service logs and metrics rather than silently disabling tracing.
func TestDecodeObservabilityResult_GenericQueryErrorPropagates(t *testing.T) {
	want := errors.New("simulated DB outage")
	got, err := decodeObservabilityResult(nil, want)
	if got != nil {
		t.Errorf("generic err path must return nil Config; got: %+v", got)
	}
	if !errors.Is(err, want) {
		t.Errorf("err must propagate the original; got: %v", err)
	}
}

// TestDecodeObservabilityResult_MalformedJSONFallsBackToDefault covers
// the lenient malformed-JSON branch — an operator who hand-edits the
// row to an invalid blob must NOT crash startup. The loader explicitly
// chooses telemetry-disabled defaults with nil error so the compliance
// proxy boots and the operator can fix the row from the admin UI.
func TestDecodeObservabilityResult_MalformedJSONFallsBackToDefault(t *testing.T) {
	got, err := decodeObservabilityResult([]byte("not json"), nil)
	if err != nil {
		t.Fatalf("malformed JSON must NOT block startup; got err: %v", err)
	}
	if got == nil {
		t.Fatal("malformed JSON path must return a non-nil default Config")
	}
	if got.Enabled {
		t.Errorf("malformed JSON fallback must leave telemetry disabled, not preserve a partial parse")
	}
	if got.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("service name not stamped on fallback: %q", got.ServiceName)
	}
}

// TestDecodeObservabilityResult_SuccessDecodesEnabledAndSamplingRate
// covers the happy path — fields land where the admin wrote them.
func TestDecodeObservabilityResult_SuccessDecodesEnabledAndSamplingRate(t *testing.T) {
	raw := []byte(`{"otelEnabled":true,"samplingRate":0.25}`)
	got, err := decodeObservabilityResult(raw, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !got.Enabled {
		t.Errorf("Enabled must reflect otelEnabled=true")
	}
	if got.SamplingRate != 0.25 {
		t.Errorf("SamplingRate not threaded: got %v, want 0.25", got.SamplingRate)
	}
	if got.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("service name must always be stamped, got %q", got.ServiceName)
	}
}

// TestDecodeObservabilityResult_EmptyJSONObjectYieldsDefaults covers the
// edge case where the admin writes a literal {} — JSON decodes cleanly
// to zero values so Enabled stays false and SamplingRate stays 0.
func TestDecodeObservabilityResult_EmptyJSONObjectYieldsDefaults(t *testing.T) {
	got, err := decodeObservabilityResult([]byte(`{}`), nil)
	if err != nil {
		t.Fatalf("empty object must decode cleanly: %v", err)
	}
	if got.Enabled || got.SamplingRate != 0 {
		t.Errorf("empty object must give zero values: %+v", got)
	}
}
