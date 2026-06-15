//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestBreakGlassVersionConflict_HTTP exercises the Hub's break-glass
// reconciliation rule over the real HTTP surface — POST
// /api/internal/things/shadow/break-glass (the dedicated break-glass route the
// production thingclient HTTP fallback posts to). The existing internal unit
// (break_glass_test.go) calls Manager.HandleShadowReport directly; this test
// verifies the BreakGlassReport handler validates the payload, forwards it to
// the Manager, and the Manager upserts the killswitch template at the reported
// version and emits an emergency_override audit event.
//
// Scenario:
//  1. The template for (compliance-proxy, killswitch) is bumped to version N
//     via the normal admin notify path.
//  2. A break-glass shadow report arrives over HTTP with reported_ver = N + 7
//     (simulates a data-plane Thing that flipped the flag locally while the
//     Hub was unreachable, then reconnected).
//  3. Hub upserts the template at N+7 and emits an emergency_override audit
//     event with actor_id = "break-glass:<tokenID>" and source_ip propagated.
func TestBreakGlassVersionConflict_HTTP(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	h := newTestHarness(t)

	const (
		thingID   = "proxy-bg-version-conflict"
		thingType = "compliance-proxy"
		configKey = "killswitch"
		tokenID   = "deadbeef"
	)

	h.cleanupConfigState(t, thingType, configKey)

	// Pre-create the Thing row so HandleShadowReport's GetThing call
	// succeeds. We don't need a live WS connection — the break-glass path
	// flows through the HTTP shadow endpoint directly.
	_ = h.hub.IssueEnrollmentTokenOfType(t, thingID, thingType)

	// Seed the template at a known baseline via the regular notify path.
	h.notifyConfigChange(t, thingType, configKey,
		map[string]any{"engaged": false}, "update")

	waitUntil(t, 5*time.Second, "template seeded", func() bool {
		_, ok := h.queryTemplateVersion(t, thingType, configKey)
		return ok
	})
	baseVer, _ := h.queryTemplateVersion(t, thingType, configKey)
	if baseVer < 1 {
		t.Fatalf("baseline version = %d, want >= 1", baseVer)
	}

	target := baseVer + 7

	// Fire a break-glass shadow report over HTTP. The shadow payload
	// reports the flipped killswitch and a key_versions map claiming the
	// new version for the killswitch key.
	h.sendBreakGlassReportHTTP(t, map[string]any{
		"id":           thingID,
		"reported":     map[string]any{configKey: map[string]any{"engaged": true}},
		"reportedVer":  target,
		"keyVersions":  map[string]int64{configKey: target},
		"sourceIp":     "10.0.0.9",
		"actorTokenId": tokenID,
	})

	// Hub must upsert the template at the reported version.
	waitUntil(t, 5*time.Second, "template upserted to break-glass version", func() bool {
		v, ok := h.queryTemplateVersion(t, thingType, configKey)
		return ok && v == target
	})

	// The most recent change event must be the emergency_override row — not
	// the earlier regular update. Assert all the forensic fields landed.
	row := h.queryLatestChangeEvent(t, thingType, configKey)
	if !row.EmergencyOverride {
		t.Fatal("latest event must have emergency_override=true")
	}
	if row.NewVersion != target {
		t.Errorf("event.new_version = %d, want %d", row.NewVersion, target)
	}
	if row.ActorID != "break-glass:"+tokenID {
		t.Errorf("event.actor_id = %q, want break-glass:%s", row.ActorID, tokenID)
	}
	if row.Action != "emergency_override" {
		t.Errorf("event.action = %q, want emergency_override", row.Action)
	}
	if row.SourceIP != "10.0.0.9" {
		t.Errorf("event.source_ip = %q, want 10.0.0.9", row.SourceIP)
	}
}
