package main

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// TestBuildConfigLoader_AllKeysRegistered locks in the registered key
// list. The agent consumes 10 shadow keys (diag_mode is a per-thing Cat A
// override carrying the diagnostic-mode window). A future edit that silently
// drops a known key would degrade the agent back to "ignore unknown" and Hub
// would never see it apply.
func TestBuildConfigLoader_AllKeysRegistered(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := thingclient.NewOutcomeTracker()
	noopApply := func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, nil
	}

	loader, _ := buildConfigLoader(configDispatchDeps{
		Logger:      logger,
		ThingID:     "test-agent",
		Outcomes:    tracker,
		HubHTTPURL:  "http://localhost:0",
		DeviceToken: "test-token",

		// Cat A
		Exemptions:    noopApply,
		KillSwitch:    noopApply,
		AgentSettings: noopApply,
		DiagMode:      noopApply,
		// Cat B
		InterceptionDomains: noopApply,
		HookConfig:          noopApply,
		PayloadCapture:      noopApply,
		StreamingCompliance: noopApply,
		InstalledRulePacks:  noopApply,
		UserContext:         noopApply,
	})

	want := []string{
		"agent_settings",
		"diag_mode", // per-thing Cat A override (diagnostic-mode window)
		"exemptions",
		"hooks",
		"installed_rule_packs",
		"interception_domains",
		"killswitch",
		"payload_capture",
		"streaming_compliance",
		"user_context",
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

// TestBuildConfigLoader_CatBKeysHaveNeedsPull locks in which keys must
// HTTP-pull from Hub. Post-PR-8 the agent consumes 7 Cat B keys —
// `exemptions` was reclassified from Cat A to Cat B because the CP
// write path is InvalidateConfig (signal-only), so the agent must
// pull the live state from Hub's AgentExemptionsLoader rather than
// trust the empty WS-pushed payload.
func TestBuildConfigLoader_CatBKeysHaveNeedsPull(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := thingclient.NewOutcomeTracker()
	noopApply := func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, nil
	}

	loader, _ := buildConfigLoader(configDispatchDeps{
		Logger:   logger,
		ThingID:  "test-agent",
		Outcomes: tracker,

		Exemptions:          noopApply,
		KillSwitch:          noopApply,
		AgentSettings:       noopApply,
		DiagMode:            noopApply,
		InterceptionDomains: noopApply,
		HookConfig:          noopApply,
		PayloadCapture:      noopApply,
		StreamingCompliance: noopApply,
		InstalledRulePacks:  noopApply,
		UserContext:         noopApply,
	})

	want := []string{
		"exemptions",
		"hooks",
		"installed_rule_packs",
		"interception_domains",
		"payload_capture",
		"streaming_compliance",
		"user_context",
	}
	got := loader.PullKeys()
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("PullKeys: got %v (n=%d), want %v (n=%d)", got, len(got), want, len(want))
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("PullKey #%d: got %q, want %q", i, got[i], k)
		}
	}
}
