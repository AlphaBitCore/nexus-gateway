package configdispatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// newTestLogger returns a logger that discards all output. Tests that care
// about log side-effects use separate assertions on observable state, not on
// the log lines themselves.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestClient constructs a thingclient.Client that will never connect (no
// Start call) but already has its OutcomeTracker allocated. This lets tests
// call tc.Outcomes() without touching the network.
//
// Each call uses a fresh prometheus.Registry to avoid "duplicate collector
// registration" panics when multiple tests create clients inside the same
// test binary.
func newTestClient(t *testing.T) *thingclient.Client {
	t.Helper()
	tc, err := thingclient.New(thingclient.Config{
		HubURL:            "http://127.0.0.1:19999", // unreachable; we never call Start
		ThingType:         "control-plane",
		ThingID:           "test-cp",
		Token:             "test-token",
		Logger:            newTestLogger(),
		MetricsRegisterer: prometheus.NewRegistry(), // isolated; avoids duplicate-registration panics
	})
	if err != nil {
		t.Fatalf("thingclient.New: %v", err)
	}
	return tc
}

// configState builds a thingclient.ConfigState from a JSON-marshallable value.
func configState(t *testing.T, v any, ver int64) thingclient.ConfigState {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return thingclient.ConfigState{State: raw, Version: ver}
}

// TestBuildConfigLoader_AllKeysRegistered locks in the registered key
// list so a future edit cannot silently drop coverage of a known
// shadow key. CP only consumes 2 keys today; both must be present.
func TestBuildConfigLoader_AllKeysRegistered(t *testing.T) {
	loader := buildConfigLoader(configDispatchDeps{
		Logger:            newTestLogger(),
		ThingID:           "test-cp",
		Outcomes:          thingclient.NewOutcomeTracker(),
		TelemetryProvider: nil, // handler tolerates nil
		DB:                nil, // observability handler only touches DB when tp != nil
		BootstrapConfig:   nil,
	})

	want := []string{"log_level", "observability"}
	got := loader.Keys()
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("registered keys: got %v (n=%d), want %v (n=%d)", got, len(got), want, len(want))
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("registered key #%d: got %q, want %q", i, got[i], k)
		}
	}
}

// registerCPLogLevel — handler apply path

// TestRegisterCPLogLevel_ValidLevel verifies that a well-formed log_level
// shadow payload atomically changes the process-wide log level. The
// observable state change is logging.CurrentLevel(), which the Apply function
// mutates via logging.SetLevel.
func TestRegisterCPLogLevel_ValidLevel(t *testing.T) {
	// Reset the level to info when the sub-test exits so parallel tests
	// that inspect logging.CurrentLevel() see a deterministic baseline.
	defer logging.SetLevel("info")

	loader := buildConfigLoader(configDispatchDeps{
		Logger:   newTestLogger(),
		ThingID:  "test-cp",
		Outcomes: thingclient.NewOutcomeTracker(),
	})

	desired := map[string]thingclient.ConfigState{
		"log_level": configState(t, map[string]string{"level": "debug"}, 1),
	}
	reported, err := loader.Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply: unexpected error: %v", err)
	}
	if _, ok := reported["log_level"]; !ok {
		t.Fatalf("reported map missing log_level key")
	}
	if got := logging.CurrentLevel(); got != slog.LevelDebug {
		t.Errorf("current level = %v, want DEBUG", got)
	}
}

// TestRegisterCPLogLevel_UnknownLevelFallsBackToInfo verifies that an
// unrecognised level string degrades gracefully to Info (the ParseLevel
// contract) rather than returning an error.
func TestRegisterCPLogLevel_UnknownLevelFallsBackToInfo(t *testing.T) {
	defer logging.SetLevel("info")

	loader := buildConfigLoader(configDispatchDeps{
		Logger:   newTestLogger(),
		ThingID:  "test-cp",
		Outcomes: thingclient.NewOutcomeTracker(),
	})

	desired := map[string]thingclient.ConfigState{
		"log_level": configState(t, map[string]string{"level": "supersonic"}, 2),
	}
	_, err := loader.Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply: unexpected error on unknown level: %v", err)
	}
	if got := logging.CurrentLevel(); got != slog.LevelInfo {
		t.Errorf("current level = %v, want INFO after unknown level string", got)
	}
}

// TestRegisterCPLogLevel_MalformedJSON verifies that a non-JSON payload
// surfaces as a loader error (parse failure) and does NOT change the level.
func TestRegisterCPLogLevel_MalformedJSON(t *testing.T) {
	defer logging.SetLevel("info")
	logging.SetLevel("info") // start deterministically at info

	loader := buildConfigLoader(configDispatchDeps{
		Logger:   newTestLogger(),
		ThingID:  "test-cp",
		Outcomes: thingclient.NewOutcomeTracker(),
	})

	desired := map[string]thingclient.ConfigState{
		"log_level": {State: []byte("not-json"), Version: 3},
	}
	_, err := loader.Apply(context.Background(), desired)
	if err == nil {
		t.Fatal("Apply: expected error on malformed JSON, got nil")
	}
	if got := logging.CurrentLevel(); got != slog.LevelInfo {
		t.Errorf("current level changed to %v, want INFO (level must not change on parse error)", got)
	}
}

// TestRegisterCPLogLevel_EmptyPayload verifies that an empty state payload
// (Hub occasionally sends empty bytes for keys not yet materialised) does
// NOT fail the apply. ParseJSON treats empty as "no-op", leaving the level
// unchanged.
func TestRegisterCPLogLevel_EmptyPayload(t *testing.T) {
	defer logging.SetLevel("info")
	logging.SetLevel("info")

	loader := buildConfigLoader(configDispatchDeps{
		Logger:   newTestLogger(),
		ThingID:  "test-cp",
		Outcomes: thingclient.NewOutcomeTracker(),
	})

	desired := map[string]thingclient.ConfigState{
		"log_level": {State: []byte{}, Version: 4},
	}
	if _, err := loader.Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply: unexpected error on empty payload: %v", err)
	}
	// Level stays at Info; Apply ran (no error) because ParseJSON[cpLogLevelState]
	// returns the zero struct on empty input and Apply calls logging.SetLevel("").
	// ParseLevel("") maps to Info, so the observable level is still Info.
	if got := logging.CurrentLevel(); got != slog.LevelInfo {
		t.Errorf("current level = %v, want INFO after empty payload", got)
	}
}

// registerCPObservability — handler apply path

// TestRegisterCPObservability_NilProviderIsNoOp verifies that when the
// telemetry provider is nil the handler returns (nil, nil) — no panic, no
// error. The observability key is still present in the loader's key table.
func TestRegisterCPObservability_NilProviderIsNoOp(t *testing.T) {
	loader := buildConfigLoader(configDispatchDeps{
		Logger:            newTestLogger(),
		ThingID:           "test-cp",
		Outcomes:          thingclient.NewOutcomeTracker(),
		TelemetryProvider: nil, // nil: guard must short-circuit
		DB:                nil,
		BootstrapConfig:   nil,
	})

	desired := map[string]thingclient.ConfigState{
		"observability": {State: []byte(`{}`), Version: 5},
	}
	reported, err := loader.Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply: unexpected error: %v", err)
	}
	// When apply returns (nil, nil) the loader echoes the desired bytes as
	// reported. Reported entry should be present.
	if _, ok := reported["observability"]; !ok {
		t.Fatalf("reported map missing observability key")
	}
}

// TestRegisterCPObservability_WithProvider verifies that when a real
// SwappableTracerProvider is wired the handler calls Reconfigure without
// error. We use nil DB and an empty BootstrapConfig so LoadOtelConfig returns
// a disabled/noop config that never dials an OTEL endpoint.
func TestRegisterCPObservability_WithProvider(t *testing.T) {
	tp, err := telemetry.Init(context.Background(), telemetry.Config{Enabled: false}, newTestLogger())
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}

	loader := buildConfigLoader(configDispatchDeps{
		Logger:            newTestLogger(),
		ThingID:           "test-cp",
		Outcomes:          thingclient.NewOutcomeTracker(),
		TelemetryProvider: tp,
		DB:                nil,              // LoadOtelConfig skips DB path when nil
		BootstrapConfig:   &config.Config{}, // empty → Enabled=false, no endpoint
	})

	desired := map[string]thingclient.ConfigState{
		"observability": {State: []byte(`{}`), Version: 6},
	}
	_, err = loader.Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply: unexpected error from observability handler with real provider: %v", err)
	}
}

// BuildConfigChangedCallback — outer wiring function

// TestBuildConfigChangedCallback_RecordsKeyStatesAndAppliesHandlers is the
// end-to-end test for the public callback factory. It verifies:
//  1. The KeyStateRecorder.Record is called for every key in desired.
//  2. Known keys (log_level, observability) are dispatched to their handlers.
//  3. Unknown keys are default-echoed into reported (the fallback branch).
//  4. The returned reported map contains entries for every key.
func TestBuildConfigChangedCallback_RecordsKeyStatesAndAppliesHandlers(t *testing.T) {
	defer logging.SetLevel("info")

	tc := newTestClient(t)
	rec := runtimeintrospect.NewKeyStateRecorder()
	logger := newTestLogger()
	cfg := &config.Config{}

	cb := BuildConfigChangedCallback(logger, "test-cp", tc, nil, nil, cfg, rec)

	logLevelPayload, _ := json.Marshal(map[string]string{"level": "warn"})
	desired := map[string]thingclient.ConfigState{
		"log_level":     {State: logLevelPayload, Version: 10},
		"observability": {State: []byte(`{}`), Version: 11},
		"unknown_key":   {State: []byte(`{"x":1}`), Version: 12},
	}

	reported, err := cb(desired)
	if err != nil {
		t.Fatalf("callback: unexpected error: %v", err)
	}

	// All three keys must appear in reported.
	for _, k := range []string{"log_level", "observability", "unknown_key"} {
		if _, ok := reported[k]; !ok {
			t.Errorf("reported map missing key %q", k)
		}
	}

	// log_level handler changed the level to warn.
	if got := logging.CurrentLevel(); got != slog.LevelWarn {
		t.Errorf("log level = %v, want WARN", got)
	}

	// KeyStateRecorder must have seen every desired key.
	for k := range desired {
		src := rec.Source(k)
		val, srcErr := src.Snapshot(context.Background())
		if srcErr != nil {
			t.Errorf("Source(%q).Snapshot: %v", k, srcErr)
		}
		// non-nil value means the state was recorded.
		if val == nil {
			t.Errorf("KeyStateRecorder has no state for key %q", k)
		}
	}
}

// TestBuildConfigChangedCallback_EmptyDesiredMap verifies that calling the
// callback with an empty desired map is valid and returns an empty reported
// map with no error.
func TestBuildConfigChangedCallback_EmptyDesiredMap(t *testing.T) {
	tc := newTestClient(t)
	rec := runtimeintrospect.NewKeyStateRecorder()

	cb := BuildConfigChangedCallback(
		newTestLogger(), "test-cp", tc, nil, nil, &config.Config{}, rec,
	)

	reported, err := cb(map[string]thingclient.ConfigState{})
	if err != nil {
		t.Fatalf("callback: unexpected error on empty desired: %v", err)
	}
	if len(reported) != 0 {
		t.Errorf("reported map len = %d, want 0", len(reported))
	}
}
