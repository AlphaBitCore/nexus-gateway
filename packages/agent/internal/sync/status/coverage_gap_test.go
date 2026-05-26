package status

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// Setter coverage — every Set* method must (a) be visible in Collect()
// output and (b) be safe to call from multiple goroutines without
// triggering -race.

// TestSetThingClient_PropagatesToConfigSummary pins that calling
// SetThingClient after construction makes Collect() pick up the new
// accessor's DesiredVer/ReportedVer in the emitted snapshot's
// ConfigSummary. Mirrors main.go ordering where the thingclient is
// constructed AFTER the status collector.
func TestSetThingClient_PropagatesToConfigSummary(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	// No accessor wired yet → ConfigSummary zeroed.
	pre := c.Collect()
	if pre.ConfigSummary.ThingVersion != 0 {
		t.Fatalf("pre-set: want zero ThingVersion, got %d", pre.ConfigSummary.ThingVersion)
	}
	c.SetThingClient(&fakeThing{desiredVer: 42, reportedVer: 42, last: "2026-04-20T10:00:00Z"})
	post := c.Collect()
	if post.ConfigSummary.ThingVersion != 42 || post.ConfigSummary.ReportedVersion != 42 {
		t.Errorf("post-set: want 42/42, got %d/%d",
			post.ConfigSummary.ThingVersion, post.ConfigSummary.ReportedVersion)
	}
	if !post.ConfigSummary.InSync {
		t.Error("want InSync=true when desired==reported")
	}
}

// TestSetSnapshotCacheGetter_OverridesThingClientForCatB pins that
// values from the cacheGetFn take priority over the thingclient desired
// snapshot for HTTP-pulled Cat B keys (interception_domains,
// hooks). This is the bug-fix path documented in the comment
// — without the cache getter the menu-bar reports 0 even when 64
// domains are live.
func TestSetSnapshotCacheGetter_OverridesThingClientForCatB(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	// thingclient says 1 enabled hook.
	c.SetThingClient(&fakeThing{
		desiredVer:  1,
		reportedVer: 1,
		desired: map[string]thingclient.ConfigState{
			"hooks": {State: json.RawMessage(`{"hookConfigs":[{"enabled":true}]}`)},
		},
	})
	// Cache says 3 enabled hooks — cache must win.
	c.SetSnapshotCacheGetter(func(key string) json.RawMessage {
		if key == "hooks" {
			return json.RawMessage(`{"hookConfigs":[{"enabled":true},{"enabled":true},{"enabled":true}]}`)
		}
		return nil
	})
	snap := c.Collect()
	if snap.ConfigSummary.HooksEnabled != 3 {
		t.Errorf("cache getter must override thingclient for hooks; want 3 got %d",
			snap.ConfigSummary.HooksEnabled)
	}
}

// TestSetSnapshotCacheGetter_NilGetterFallsBackToThingClient pins that
// passing nil from the cache getter falls through to the thingclient
// snapshot — the pick() helper's len(v) == 0 branch.
func TestSetSnapshotCacheGetter_NilGetterFallsBackToThingClient(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	c.SetThingClient(&fakeThing{
		desiredVer:  1,
		reportedVer: 1,
		desired: map[string]thingclient.ConfigState{
			"hooks": {State: json.RawMessage(`{"hookConfigs":[{"enabled":true}]}`)},
		},
	})
	c.SetSnapshotCacheGetter(func(key string) json.RawMessage {
		return nil // always miss → must fall through to thingclient
	})
	snap := c.Collect()
	if snap.ConfigSummary.HooksEnabled != 1 {
		t.Errorf("cache-miss must fall back to thingclient; want 1 got %d",
			snap.ConfigSummary.HooksEnabled)
	}
}

// TestSetTodayStatsFn_RefreshesEveryCollect locks the contract that
// the collector RE-INVOKES todayStatsFn on each Collect() — the audit
// roll-up changes between polls and the snapshot must reflect it.
func TestSetTodayStatsFn_RefreshesEveryCollect(t *testing.T) {
	calls := 0
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	c.SetTodayStatsFn(func() TodayStats {
		calls++
		return TodayStats{Inspected: calls * 10, Passthrough: calls * 2, Denied: calls}
	})
	first := c.Collect()
	second := c.Collect()
	if calls != 2 {
		t.Errorf("want todayStatsFn called once per Collect (got %d)", calls)
	}
	if first.TodayStats.Inspected != 10 || second.TodayStats.Inspected != 20 {
		t.Errorf("want refreshed counters across Collect calls; got %d then %d",
			first.TodayStats.Inspected, second.TodayStats.Inspected)
	}
}

// TestSetRecentEventsFn_LimitFiveAndPropagation pins the limit value
// (5, per main.go's "Recent activity" budget) and that whatever the
// callback returns lands verbatim in the snapshot.
func TestSetRecentEventsFn_LimitFiveAndPropagation(t *testing.T) {
	var seenLimit int
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	c.SetRecentEventsFn(func(limit int) []RecentEvent {
		seenLimit = limit
		return []RecentEvent{
			{Time: "2026-04-20T10:00:00Z", ProcessName: "curl", DestHost: "api.openai.com", Action: "inspect"},
			{Time: "2026-04-20T10:00:01Z", ProcessName: "claude-cli", DestHost: "api.anthropic.com", Action: "allow"},
		}
	})
	snap := c.Collect()
	if seenLimit != 5 {
		t.Errorf("want recentEventsFn invoked with limit=5, got %d", seenLimit)
	}
	if len(snap.RecentEvents) != 2 || snap.RecentEvents[0].DestHost != "api.openai.com" {
		t.Errorf("recent events not propagated; got %+v", snap.RecentEvents)
	}
}

// TestSetInterceptionHealthFn_AfterConstruction pins that wiring the
// health provider AFTER NewCollector — the production ordering, where
// the platform shim initializes later than the status layer — feeds
// into computeState on the next Collect.
func TestSetInterceptionHealthFn_AfterConstruction(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	// No health provider → active.
	if got := c.Collect().State; got != "active" {
		t.Fatalf("pre-wire: want active, got %s", got)
	}
	// Wire after the fact: zero connections + past grace period → degraded.
	c.SetInterceptionHealthFn(func() InterceptionHealth {
		return InterceptionHealth{
			StartedAt:        time.Now().Add(-2 * InterceptionGracePeriod),
			ConnectionsTotal: 0,
		}
	})
	snap := c.Collect()
	if snap.State != "degraded" || snap.StateReason != "Network filter not connected" {
		t.Errorf("post-wire: want degraded/Network filter not connected, got %s/%s",
			snap.State, snap.StateReason)
	}
}

// TestSetLastHeartbeat_VisibleInAgentSection pins that the heartbeat
// time written via the setter surfaces in AgentInfo.LastHeartbeat as
// an RFC3339 string.
func TestSetLastHeartbeat_VisibleInAgentSection(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	stamp := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	c.SetLastHeartbeat(stamp)
	got := c.Collect().Agent.LastHeartbeat
	if got != "2026-04-20T10:00:00Z" {
		t.Errorf("want 2026-04-20T10:00:00Z, got %q", got)
	}
}

// TestMarkProviderTraffic_StampsLastProviderTrafficAt pins the live-
// traffic pulse path: the moment a provider-tagged audit event lands
// the snapshot's lastProviderTrafficAt must be non-empty AND lie
// within a tight window of "now".
func TestMarkProviderTraffic_StampsLastProviderTrafficAt(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	before := c.Collect().Agent.LastProviderTrafficAt
	if before != "" {
		t.Fatalf("pre-mark: want empty, got %q", before)
	}
	c.MarkProviderTraffic()
	got := c.Collect().Agent.LastProviderTrafficAt
	if got == "" {
		t.Fatal("post-mark: want non-empty timestamp")
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("post-mark: timestamp not RFC3339: %v", err)
	}
	if delta := time.Since(parsed); delta < 0 || delta > 5*time.Second {
		t.Errorf("post-mark: timestamp %v not within 5s of now (delta=%v)", parsed, delta)
	}
}

// TestSetUpdateAvailable_PropagatesToAgentInfo pins that the update-
// available flag (set by the updater poller) lands in
// AgentInfo.UpdateAvailable verbatim.
func TestSetUpdateAvailable_PropagatesToAgentInfo(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	if c.Collect().Agent.UpdateAvailable {
		t.Fatal("pre-set: want false")
	}
	c.SetUpdateAvailable(true)
	if !c.Collect().Agent.UpdateAvailable {
		t.Error("post-set true: want true")
	}
	c.SetUpdateAvailable(false)
	if c.Collect().Agent.UpdateAvailable {
		t.Error("post-set false: want false")
	}
}

// TestSetShutdownWarning_DefensiveCopy pins the defensive-copy
// contract: mutating the original map AFTER SetShutdownWarning must
// not change the snapshot's payload. This stops a caller-side rewrite
// from leaking through the menu-bar UI's locale dictionary.
func TestSetShutdownWarning_DefensiveCopy(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	src := map[string]string{
		"en": "Shutting down disables compliance protections.",
		"zh": "关闭代理将禁用合规保护。",
		"es": "Apagar deshabilita las protecciones de cumplimiento.",
	}
	c.SetShutdownWarning(src)
	// Mutate the caller's reference.
	src["en"] = "HACKED"
	delete(src, "zh")

	snap := c.Collect()
	if snap.ShutdownWarning["en"] != "Shutting down disables compliance protections." {
		t.Errorf("defensive copy broken: caller mutation leaked, got %q", snap.ShutdownWarning["en"])
	}
	if _, ok := snap.ShutdownWarning["zh"]; !ok {
		t.Error("defensive copy broken: caller delete leaked")
	}
	if len(snap.ShutdownWarning) != 3 {
		t.Errorf("defensive copy lost entries: want 3, got %d", len(snap.ShutdownWarning))
	}
}

// TestSetShutdownWarning_NilClears pins the nil-clear path: passing
// nil drops the snapshot back to nil rather than to a zero-length map
// (matches the JSON contract — `shutdownWarning: null` not `{}`).
func TestSetShutdownWarning_NilClears(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	c.SetShutdownWarning(map[string]string{"en": "warning"})
	c.SetShutdownWarning(nil)
	if c.Collect().ShutdownWarning != nil {
		t.Error("nil set: want nil shutdownWarning")
	}
}

// computeState / checkInterceptionHealth — degraded vs grace-period vs
// healthy arms.

// TestCheckInterceptionHealth_StillWarmingUp pins the grace-period
// fail-OPEN behaviour: even with zero connections, an agent that just
// started must NOT be reported as degraded (the NE extension needs a
// few seconds to attach). Uses NowFn to fast-forward deterministically.
func TestCheckInterceptionHealth_StillWarmingUp(t *testing.T) {
	startedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	c := NewCollector(CollectorConfig{
		CertExpiresAt:        time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn:      func() int { return 0 },
		InterceptionHealthFn: func() InterceptionHealth { return InterceptionHealth{StartedAt: startedAt} },
		NowFn:                func() time.Time { return startedAt.Add(InterceptionGracePeriod / 2) },
	})
	snap := c.Collect()
	if snap.State != "active" {
		t.Errorf("during grace period must stay active; got %s/%s", snap.State, snap.StateReason)
	}
}

// TestCheckInterceptionHealth_DetachedReason pins the second failure-
// mode reason — at least one attach has happened (ConnectionsTotal>0)
// but the current session count is zero. Surfaces "Network filter
// detached" so the user can re-enable the system extension.
func TestCheckInterceptionHealth_DetachedReason(t *testing.T) {
	startedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
		InterceptionHealthFn: func() InterceptionHealth {
			return InterceptionHealth{
				StartedAt:        startedAt,
				ConnectionsTotal: 100, // attach has happened
				ActiveSessions:   0,   // but currently detached
			}
		},
		NowFn: func() time.Time { return startedAt.Add(2 * InterceptionGracePeriod) },
	})
	snap := c.Collect()
	if snap.State != "degraded" || snap.StateReason != "Network filter detached" {
		t.Errorf("want degraded/Network filter detached, got %s/%s", snap.State, snap.StateReason)
	}
}

// TestCheckInterceptionHealth_Healthy pins the all-good path: past
// grace period, both counters non-zero. State must stay active so
// other computeState arms (cert expiry, queue backlog) can drive the
// final verdict.
func TestCheckInterceptionHealth_Healthy(t *testing.T) {
	startedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
		InterceptionHealthFn: func() InterceptionHealth {
			return InterceptionHealth{
				StartedAt:        startedAt,
				ConnectionsTotal: 100,
				ActiveSessions:   3,
			}
		},
		NowFn: func() time.Time { return startedAt.Add(2 * InterceptionGracePeriod) },
	})
	snap := c.Collect()
	if snap.State != "active" || snap.StateReason != "" {
		t.Errorf("want active, got %s/%s", snap.State, snap.StateReason)
	}
}

// TestCheckInterceptionHealth_ZeroStartedAtSkipsGraceCheck pins the
// "started-at zero" path: providers that haven't recorded a start
// timestamp skip the grace-period guard and fall straight into the
// connections check. Matches platform shims that emit a synthetic
// zero-time before first attach.
func TestCheckInterceptionHealth_ZeroStartedAtSkipsGraceCheck(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
		InterceptionHealthFn: func() InterceptionHealth {
			return InterceptionHealth{StartedAt: time.Time{}, ConnectionsTotal: 0}
		},
	})
	snap := c.Collect()
	if snap.State != "degraded" || snap.StateReason != "Network filter not connected" {
		t.Errorf("zero StartedAt should skip grace and surface not-connected; got %s/%s",
			snap.State, snap.StateReason)
	}
}

// TestCheckInterceptionHealth_NowFnUnsetUsesRealClock pins that when
// nowFn is left nil the code path falls back to time.Now — and a
// well-past startedAt still cleanly resolves to a not-connected
// degraded state (no grace window).
func TestCheckInterceptionHealth_NowFnUnsetUsesRealClock(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
		InterceptionHealthFn: func() InterceptionHealth {
			return InterceptionHealth{
				StartedAt:        time.Now().Add(-2 * InterceptionGracePeriod),
				ConnectionsTotal: 0,
			}
		},
		// NowFn intentionally unset → exercises the time.Now fallback.
	})
	snap := c.Collect()
	if snap.State != "degraded" {
		t.Errorf("real-clock fallback should still degrade; got %s", snap.State)
	}
}

// Cat A killSwitch / Cat B rulePacks payload parsing — happy + malformed
// + every wire-shape branch.

// TestParseKillSwitchBrief_EngagedWhenWireEngagedTrue pins the canonical
// path: the shadow's `engaged=true` means the kill switch is engaged
// (traffic is paused / passthrough), so the brief surfaces engaged=true.
// Includes actor + reason round-trip.
func TestParseKillSwitchBrief_EngagedWhenWireEngagedTrue(t *testing.T) {
	raw := json.RawMessage(`{"engaged":true,"actor":"admin","reason":"compliance audit"}`)
	got := parseKillSwitchBrief(raw)
	if !got.Engaged {
		t.Error("want engaged=true when wire engaged=true")
	}
	if got.Actor != "admin" || got.Reason != "compliance audit" {
		t.Errorf("actor/reason not round-tripped: got actor=%q reason=%q", got.Actor, got.Reason)
	}
}

// TestParseKillSwitchBrief_NotEngagedWhenWireEngagedFalse pins the happy
// path: wire engaged=false means normal interception, so the brief
// surfaces engaged=false.
func TestParseKillSwitchBrief_NotEngagedWhenWireEngagedFalse(t *testing.T) {
	raw := json.RawMessage(`{"engaged":false,"actor":"user-paused"}`)
	got := parseKillSwitchBrief(raw)
	if got.Engaged {
		t.Error("want engaged=false when wire engaged=false")
	}
	if got.Actor != "user-paused" {
		t.Errorf("actor not preserved: got %q", got.Actor)
	}
}

// TestParseKillSwitchBrief_EngagedOmittedTreatedAsNotEngaged pins
// the `engaged` nil-pointer arm — Hub legacy publishers send neither
// true nor false. Default is engaged=false (don't false-alarm the UI).
func TestParseKillSwitchBrief_EngagedOmittedTreatedAsNotEngaged(t *testing.T) {
	raw := json.RawMessage(`{"actor":"fleet"}`)
	got := parseKillSwitchBrief(raw)
	if got.Engaged {
		t.Error("want engaged=false when engaged field missing")
	}
	if got.Actor != "fleet" {
		t.Errorf("actor not preserved: got %q", got.Actor)
	}
}

// TestParseKillSwitchBrief_EmptyPayload pins the "no shadow yet" arm
// returning a zero-value brief — UI renders the "off" state.
func TestParseKillSwitchBrief_EmptyPayload(t *testing.T) {
	got := parseKillSwitchBrief(nil)
	if got != (KillSwitchBrief{}) {
		t.Errorf("want zero-value, got %+v", got)
	}
}

// TestParseKillSwitchBrief_MalformedJSON pins the fail-safe: bad JSON
// returns a zero-value brief instead of panicking or surfacing a
// half-decoded result.
func TestParseKillSwitchBrief_MalformedJSON(t *testing.T) {
	got := parseKillSwitchBrief(json.RawMessage(`{not-json`))
	if got != (KillSwitchBrief{}) {
		t.Errorf("malformed JSON should yield zero-value, got %+v", got)
	}
}

// TestParseRulePacksBrief_PrimaryRulePacksField pins the canonical
// Hub Cat B shape — {"rulePacks":[...]} — including ID/name/version
// round-trip.
func TestParseRulePacksBrief_PrimaryRulePacksField(t *testing.T) {
	raw := json.RawMessage(`{"rulePacks":[{"id":"rp-1","name":"PII Detector","version":"1.0.0"},{"id":"rp-2","name":"Secrets","version":"2.1.3"}]}`)
	got := parseRulePacksBrief(raw)
	if len(got) != 2 {
		t.Fatalf("want 2 packs, got %d", len(got))
	}
	if got[0].ID != "rp-1" || got[0].Name != "PII Detector" || got[0].Version != "1.0.0" {
		t.Errorf("entry 0 mismatch: %+v", got[0])
	}
	if got[1].ID != "rp-2" || got[1].Version != "2.1.3" {
		t.Errorf("entry 1 mismatch: %+v", got[1])
	}
}

// TestParseRulePacksBrief_LegacyPacksField pins the back-compat
// fallback — older Hub builds emit {"packs":[...]}.
func TestParseRulePacksBrief_LegacyPacksField(t *testing.T) {
	raw := json.RawMessage(`{"packs":[{"id":"legacy-1","name":"Legacy","version":"0.9"}]}`)
	got := parseRulePacksBrief(raw)
	if len(got) != 1 || got[0].ID != "legacy-1" {
		t.Errorf("want 1 legacy pack, got %+v", got)
	}
}

// TestParseRulePacksBrief_EmptyPayload pins the empty-payload arm
// returning a non-nil empty slice (matches the JSON contract — UI
// distinguishes `[]` from `null`).
func TestParseRulePacksBrief_EmptyPayload(t *testing.T) {
	got := parseRulePacksBrief(nil)
	if got == nil {
		t.Error("want non-nil empty slice for nil payload")
	}
	if len(got) != 0 {
		t.Errorf("want 0 entries, got %d", len(got))
	}
}

// TestParseRulePacksBrief_MalformedJSON pins the fail-safe.
func TestParseRulePacksBrief_MalformedJSON(t *testing.T) {
	got := parseRulePacksBrief(json.RawMessage(`{bad-json`))
	if got == nil || len(got) != 0 {
		t.Errorf("malformed JSON should yield non-nil empty slice, got %+v", got)
	}
}

// TestParseRulePacksBrief_NeitherFieldPresent pins the "valid JSON
// but no recognised key" arm — must still return an empty slice
// rather than nil.
func TestParseRulePacksBrief_NeitherFieldPresent(t *testing.T) {
	got := parseRulePacksBrief(json.RawMessage(`{"other":"value"}`))
	if got == nil || len(got) != 0 {
		t.Errorf("unknown shape should yield empty slice, got %+v", got)
	}
}

// TestParseExemptionsBrief_LegacyEntriesFallback pins the older
// {"entries":[...]} wire shape used by pre-Hub-rollout publishers.
// The parser must fall back to it when the canonical "active" key
// is empty so a rolling upgrade doesn't visually wipe the exemption
// list.
func TestParseExemptionsBrief_LegacyEntriesFallback(t *testing.T) {
	raw := json.RawMessage(`{"entries":[{"id":"e1","host":"api.openai.com","reason":"prod debug","until":"2026-04-21T00:00:00Z"}]}`)
	got := parseExemptionsBrief(raw)
	if len(got) != 1 {
		t.Fatalf("want 1 entry (legacy shape), got %d", len(got))
	}
	if got[0].Host != "api.openai.com" || got[0].Reason != "prod debug" {
		t.Errorf("entry not round-tripped: %+v", got[0])
	}
}

// TestParseExemptionsBrief_MalformedJSON pins the fail-safe: bad
// JSON returns the non-nil empty slice rather than panicking.
func TestParseExemptionsBrief_MalformedJSON(t *testing.T) {
	got := parseExemptionsBrief(json.RawMessage(`{not-json`))
	if got == nil || len(got) != 0 {
		t.Errorf("malformed JSON should yield empty slice, got %+v", got)
	}
}

// TestParseExemptionsBrief_EmptyPayload pins the no-shadow arm —
// returns the non-nil empty slice so the UI can treat the JSON
// `[]` shape as "no active exemptions".
func TestParseExemptionsBrief_EmptyPayload(t *testing.T) {
	got := parseExemptionsBrief(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("nil payload should yield non-nil empty slice, got %+v", got)
	}
}

// formatPausedUntil — nil fn, non-active flag, zero time, real value.

// TestFormatPausedUntil_NilFn pins the no-provider arm: no pause
// scheduling wired → empty string.
func TestFormatPausedUntil_NilFn(t *testing.T) {
	if got := formatPausedUntil(nil); got != "" {
		t.Errorf("nil fn: want empty, got %q", got)
	}
}

// TestFormatPausedUntil_OkFalse pins the "paused indefinitely" arm:
// even when the timer accessor returns a time value, ok=false (no
// auto-resume scheduled) collapses to empty string.
func TestFormatPausedUntil_OkFalse(t *testing.T) {
	fn := func() (time.Time, bool) { return time.Now(), false }
	if got := formatPausedUntil(fn); got != "" {
		t.Errorf("ok=false: want empty, got %q", got)
	}
}

// TestFormatPausedUntil_ZeroTime pins the zero-time arm — UI gets the
// same empty string as "not paused" so the chip doesn't render a
// 0001-01-01 deadline.
func TestFormatPausedUntil_ZeroTime(t *testing.T) {
	fn := func() (time.Time, bool) { return time.Time{}, true }
	if got := formatPausedUntil(fn); got != "" {
		t.Errorf("zero time: want empty, got %q", got)
	}
}

// TestFormatPausedUntil_RealValueRFC3339UTC pins the happy path —
// returns a UTC RFC3339 string regardless of the source location.
func TestFormatPausedUntil_RealValueRFC3339UTC(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	t0 := time.Date(2026, 4, 20, 10, 0, 0, 0, loc)
	fn := func() (time.Time, bool) { return t0, true }
	got := formatPausedUntil(fn)
	want := "2026-04-20T14:00:00Z" // EDT → UTC offset 4h
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// TestCollect_PausedSetWhenProviderReturnsTrue pins the snapshot
// surface for the kill-switch UI: PausedFn=true must land on
// snap.Paused, and the paired PausedUntilFn must populate
// PausedUntil.
func TestCollect_PausedSetWhenProviderReturnsTrue(t *testing.T) {
	resume := time.Date(2026, 4, 20, 11, 0, 0, 0, time.UTC)
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
		PausedFn:        func() bool { return true },
		PausedUntilFn:   func() (time.Time, bool) { return resume, true },
	})
	snap := c.Collect()
	if !snap.Paused {
		t.Error("want Paused=true")
	}
	if snap.PausedUntil != "2026-04-20T11:00:00Z" {
		t.Errorf("want pausedUntil=2026-04-20T11:00:00Z, got %q", snap.PausedUntil)
	}
}

// callIntFn / callStringFn / callBoolFn fn-set arms — already covered
// by nil-arm via NewCollector(zero) but we need the "non-nil and
// returns X" branch documented.

// TestCallFns_ReturnValuesPropagateThroughCollect pins all four
// optional fn providers feeding into AgentInfo. Two get rendered as
// fields; the boolean default helper carries quitAllowed=true/false.
func TestCallFns_ReturnValuesPropagateThroughCollect(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:    time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn:  func() int { return 0 },
		TrustLevelFn:     func() int { return 3 },
		DeviceAuthModeFn: func() string { return "mtls-only" },
		SSOEmailFn:       func() string { return "user@example.com" },
		QuitAllowedFn:    func() bool { return true },
	})
	snap := c.Collect()
	if snap.Agent.TrustLevel != 3 {
		t.Errorf("trust level: want 3, got %d", snap.Agent.TrustLevel)
	}
	if snap.Agent.DeviceAuthMode != "mtls-only" {
		t.Errorf("device auth mode: want mtls-only, got %q", snap.Agent.DeviceAuthMode)
	}
	if snap.Agent.SSOEmail != "user@example.com" {
		t.Errorf("sso email: want user@example.com, got %q", snap.Agent.SSOEmail)
	}
	if !snap.Agent.QuitAllowed {
		t.Error("quitAllowed=true: want true")
	}
}

// Concurrency — race-detector smoke for all the setters.

// TestCollector_ConcurrentSettersAreSafe runs every setter from many
// goroutines simultaneously while Collect() drains snapshots. -race
// would fire on any unguarded read/write. Exists primarily to lock
// the mutex contract — without it a future refactor could drop the
// mu.Lock without a unit failure.
func TestCollector_ConcurrentSettersAreSafe(t *testing.T) {
	c := newTestCollector()
	c.SetThingClient(&fakeThing{desiredVer: 1, reportedVer: 1})
	c.SetSnapshotCacheGetter(func(string) json.RawMessage { return nil })
	c.SetInterceptionHealthFn(func() InterceptionHealth { return InterceptionHealth{} })
	c.SetTodayStatsFn(func() TodayStats { return TodayStats{} })
	c.SetRecentEventsFn(func(int) []RecentEvent { return nil })

	const goroutines = 16
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 7)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.SetGatewayConnected(j%2 == 0)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.SetLastHeartbeat(time.Now())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.SetLastSyncTime(time.Now())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.MarkProviderTraffic()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.SetUpdateAvailable(j%2 == 0)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.SetShutdownWarning(map[string]string{"en": "warn"})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = c.Collect()
			}
		}()
	}
	wg.Wait()

	// Final snapshot must still be coherent.
	snap := c.Collect()
	if snap.Agent.Version == "" {
		t.Error("version dropped under concurrent load")
	}
}
