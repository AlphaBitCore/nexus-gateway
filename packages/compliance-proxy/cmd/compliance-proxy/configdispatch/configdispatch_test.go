package configdispatch_test

import (
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/configdispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// BuildConfigLoader is a wiring function — the most useful guarantee
// we can lock in without spinning Hub/DB/proxy is "every key the
// proxy used to handle in its giant switch is still registered". A
// missing key here would silently degrade the proxy back to the
// default-echo branch and Hub would never see it apply.
//
// The canonical list comes from the pre-refactor switch statement (11
// cases plus the default catch-all); the default is preserved by the
// main.go wrapper, not by the loader. PR-6 added `streaming_compliance`
// to fix the missing-receiver gap for streaming_compliance.
func TestBuildConfigLoader_AllKeysRegistered(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := thingclient.NewOutcomeTracker()

	// Construct the smallest set of dependencies that satisfies the
	// types — most subsystems can be left nil because we only check
	// the registration table, not the apply path. AccessChecker /
	// KillSwitch / ExemptionStore / ProxyServer / PayloadCaptureStore
	// / CacheManager all require concrete types in their Register
	// closures' arg lists, so we instantiate the cheap ones.
	accessChecker, err := access.NewChecker(nil, nil, nil)
	if err != nil {
		t.Fatalf("access.NewChecker: %v", err)
	}

	loader := configdispatch.BuildConfigLoader(configdispatch.Deps{
		Logger:              logger,
		ThingID:             "test-proxy",
		Outcomes:            tracker,
		KillSwitch:          killswitch.NewKillSwitch(logger),
		ExemptionStore:      exemption.NewStore(logger),
		HookConfigCache:     nil, // optional — handler tolerates nil
		ConfigDB:            nil, // optional — handler tolerates nil
		CacheManager:        cache.NewManager(0, logger),
		AccessChecker:       accessChecker,
		TelemetryProvider:   nil, // optional — handler tolerates nil
		PayloadCaptureStore: payloadcapture.NewStore(payloadcapture.DefaultConfig()),
		ProxyServer:         nil, // ProxyServer is only used by handlers that
		// require it; tests that need those handlers should re-wire.
	})

	want := []string{
		"exemptions",
		"hooks",
		"interception_domains",
		"killswitch",
		"log_level",
		"observability",
		"onboarding",
		"payload_capture",
		"streaming_compliance",
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
