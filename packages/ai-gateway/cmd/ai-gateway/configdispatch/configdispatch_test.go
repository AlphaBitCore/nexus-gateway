package configdispatch

import (
	"io"
	"log/slog"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// TestBuildConfigLoader_AllKeysRegistered locks in the registered key
// list. AI Gateway consumes 19 shadow keys (streaming_compliance was removed
// in F-0125 — it was never in ValidByThingType["ai-gateway"], so its applier
// was dead code). A future edit that silently drops a known shadow key would
// degrade the gateway back to the default-echo branch and Hub would never see
// it apply.
func TestBuildConfigLoader_AllKeysRegistered(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := thingclient.NewOutcomeTracker()

	var obs atomic.Pointer[telemetry.Config]
	loader := BuildConfigLoader(Deps{
		Logger:             logger,
		ThingID:            "test-ag",
		Outcomes:           tracker,
		ObservabilityState: &obs,
		// All other deps left nil; every handler tolerates nil and
		// short-circuits — the test only inspects the registration
		// table, not the apply path.
	})

	want := []string{
		"ai_guard",
		"cache",
		"credential_reliability",
		"credentials",
		"gateway_passthrough",
		"hooks",
		"log_level",
		"models",
		"observability",
		"organizations",
		"payload_capture",
		"providers",
		"quota_overrides",
		"quota_policies",
		"response_cache.extract_config",          // fleet extract-cache push
		"response_cache.time_sensitive_patterns", // time-sensitive cache patterns
		"routing_rules",
		"semantic_cache.config", // semantic cache config
		"virtual_keys",
	}
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
