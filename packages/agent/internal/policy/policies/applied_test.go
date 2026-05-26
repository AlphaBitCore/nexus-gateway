package policies

import (
	"encoding/json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// fakeAccessor implements ThingStateAccessor against a hand-built
// snapshot map. Lets tests drive parsing through Build without
// touching a real thingclient.Client.
type fakeAccessor struct {
	snap         map[string]thingclient.ConfigState
	desiredVer   int64
	reportedVer  int64
	lastReported string
}

func (f *fakeAccessor) SnapshotDesired() map[string]thingclient.ConfigState { return f.snap }
func (f *fakeAccessor) DesiredVer() int64                                   { return f.desiredVer }
func (f *fakeAccessor) ReportedVer() int64                                  { return f.reportedVer }
func (f *fakeAccessor) LastReportedAt() string                              { return f.lastReported }

func mustState(t *testing.T, v any) thingclient.ConfigState {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return thingclient.ConfigState{State: b}
}

func TestBuild_NilAccessorReturnsEmpty(t *testing.T) {
	got := Build(nil, nil)
	if len(got.InterceptionDomains) != 0 || len(got.Hooks) != 0 || len(got.Exemptions) != 0 {
		t.Errorf("expected empty AppliedConfig, got %+v", got)
	}
	if got.Sync.InSync {
		// Both zero versions match — InSync==true is a defensible
		// reading, but the nil-accessor path should not even attempt
		// the assignment. Today Build returns the zero SyncStatus
		// unmodified, so InSync is zero (false). Pin that.
		t.Errorf("expected Sync.InSync to be false on nil accessor")
	}
}

func TestBuild_SyncStatusReflectsThingclient(t *testing.T) {
	acc := &fakeAccessor{
		snap:         map[string]thingclient.ConfigState{},
		desiredVer:   42,
		reportedVer:  42,
		lastReported: "2026-05-13T15:00:00Z",
	}
	got := Build(acc, nil)
	if got.Sync.DesiredVersion != 42 || got.Sync.ReportedVersion != 42 {
		t.Errorf("versions: got %+v", got.Sync)
	}
	if !got.Sync.InSync {
		t.Errorf("InSync: want true (both 42), got false")
	}
	if got.Sync.LastReportedAt != "2026-05-13T15:00:00Z" {
		t.Errorf("LastReportedAt: got %q", got.Sync.LastReportedAt)
	}
}

func TestBuild_SyncStatusDriftedWhenVersionsDiffer(t *testing.T) {
	acc := &fakeAccessor{desiredVer: 10, reportedVer: 8}
	got := Build(acc, nil)
	if got.Sync.InSync {
		t.Errorf("InSync: want false (10 vs 8), got true")
	}
}

func TestParseInterceptionDomains_NewDTO(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"interception_domains": mustState(t, map[string]any{
			"interceptionDomains": []map[string]any{
				{"id": "1", "name": "OpenAI", "hostPattern": "*.openai.com", "hostMatchType": "wildcard", "enabled": true, "priority": 100, "defaultPathAction": "intercept"},
				{"id": "2", "name": "Anthropic", "hostPattern": "*.anthropic.com", "hostMatchType": "wildcard", "enabled": true},
				{"id": "3", "name": "Slack", "hostPattern": "*.slack.com", "hostMatchType": "wildcard", "enabled": false, "defaultPathAction": "passthrough"},
			},
		}),
	}}
	got := Build(acc, nil).InterceptionDomains
	if len(got) != 3 {
		t.Fatalf("want 3 domains, got %d", len(got))
	}
	if got[0].HostPattern != "*.openai.com" || got[0].Name != "OpenAI" || !got[0].Enabled || got[0].Priority != 100 {
		t.Errorf("row 0 = %+v", got[0])
	}
	if got[2].Enabled {
		t.Errorf("row 2 (slack) should reflect enabled=false from shadow")
	}
}

func TestParseInterceptionDomains_LegacyStringList(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"interception_domains": mustState(t, map[string]any{
			"domains": []string{"a.com", "b.com"},
		}),
	}}
	got := Build(acc, nil).InterceptionDomains
	if len(got) != 2 {
		t.Fatalf("want 2 (legacy shape), got %d", len(got))
	}
	if got[0].HostPattern != "a.com" || !got[0].Enabled {
		t.Errorf("legacy shape should default enabled=true")
	}
}

func TestParseHooks_ReadsFullSchema(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"hooks": mustState(t, map[string]any{
			"hookConfigs": []map[string]any{
				{"id": "pii", "implementationId": "pii-redactor", "name": "PII Redactor", "stage": "preOutbound", "priority": 1, "enabled": true, "failBehavior": "passthrough", "timeoutMs": 500, "applicableIngress": []string{"ai-gateway"}},
				{"id": "off", "name": "Disabled hook", "stage": "preInbound", "priority": 2, "enabled": false},
				{"id": "promptscan", "name": "Prompt Scanner", "stage": "preInbound", "priority": 3, "enabled": true},
			},
		}),
	}}
	got := Build(acc, nil).Hooks
	if len(got) != 3 {
		t.Fatalf("want 3 hooks (including disabled), got %d", len(got))
	}
	if got[0].ID != "pii" || got[0].Stage != "preOutbound" || got[0].Priority != 1 || got[0].FailBehavior != "passthrough" {
		t.Errorf("row 0 = %+v", got[0])
	}
	if got[1].Enabled {
		t.Errorf("disabled hook should retain Enabled=false so UI can show admin's intent")
	}
}

func TestParseKillSwitch_Engaged(t *testing.T) {
	// Wire shape: {engaged: true} = killswitch engaged (passthrough).
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"killswitch": mustState(t, map[string]any{
			"engaged": true,
			"reason":  "Q3 audit shutoff",
		}),
	}}
	got := Build(acc, nil).KillSwitch
	if !got.Engaged || got.Reason != "Q3 audit shutoff" {
		t.Errorf("KillSwitch = %+v (want Engaged=true, Reason=\"Q3 audit shutoff\")", got)
	}
}

// TestParseKillSwitch_DisengagedAfterAdminClear locks the disengaged
// path: wire engaged=false means normal interception posture.
func TestParseKillSwitch_DisengagedAfterAdminClear(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"killswitch": mustState(t, map[string]any{
			"engaged": false,
		}),
	}}
	got := Build(acc, nil).KillSwitch
	if got.Engaged || got.Reason != "" {
		t.Errorf("KillSwitch = %+v (want Engaged=false)", got)
	}
}

// TestParseKillSwitch_AbsentDefaultsDisengaged locks fail-open: an empty
// shadow payload defaults to Engaged=false (interception runs normally).
func TestParseKillSwitch_AbsentDefaultsDisengaged(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{}}
	got := Build(acc, nil).KillSwitch
	if got.Engaged {
		t.Errorf("absent shadow should default to Engaged=false (fail-open); got %+v", got)
	}
}

func TestParseDeviceDefaults_QuitAllowedAndIntervals(t *testing.T) {
	qa := true
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"agent_settings": mustState(t, map[string]any{
			"quitAllowed":           qa,
			"heartbeatIntervalSec":  60,
			"auditDrainIntervalSec": 30,
			"configSyncIntervalSec": 300,
			"auditBatchSize":        100,
			"shutdownWarning": map[string]string{
				"en": "Closing Nexus Agent will stop AI traffic monitoring.",
				"zh": "关闭 Nexus Agent 将停止本设备上的 AI 流量监控。",
			},
			"shutdownWarningEnabled": true,
			"autoUpdateChannel":      "stable",
			"autoUpdateEnabled":      true,
			"logLevel":               "info",
		}),
	}}
	got := Build(acc, nil).DeviceDefaults
	if got.QuitAllowed == nil || !*got.QuitAllowed {
		t.Errorf("QuitAllowed: %v", got.QuitAllowed)
	}
	if got.HeartbeatIntervalSec != 60 || got.AuditDrainIntervalSec != 30 || got.ConfigSyncIntervalSec != 300 {
		t.Errorf("intervals: %+v", got)
	}
	if !got.ShutdownWarningEnabled || got.ShutdownWarning["en"] == "" {
		t.Errorf("shutdownWarning: %+v", got)
	}
	if got.AutoUpdateChannel != "stable" || !got.AutoUpdateEnabled {
		t.Errorf("autoUpdate: %+v", got)
	}
}

func TestParseExemptions_ActiveKey(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"exemptions": mustState(t, map[string]any{
			"active": []map[string]any{
				{"id": "exm-1", "host": "internal.corp", "reason": "vendor allowlist"},
			},
		}),
	}}
	got := Build(acc, nil).Exemptions
	if len(got) != 1 || got[0].ID != "exm-1" || got[0].Host != "internal.corp" {
		t.Errorf("Exemptions = %+v", got)
	}
}

func TestParseDiagMode_NestedShape(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"agent_settings": mustState(t, map[string]any{
			"diagMode": map[string]any{
				"active": true,
				"until":  "2026-05-13T18:00:00Z",
			},
		}),
	}}
	got := Build(acc, nil).DiagMode
	if got == nil || !got.Active {
		t.Fatalf("DiagMode should be active, got %+v", got)
	}
	if got.Until != "2026-05-13T18:00:00Z" {
		t.Errorf("Until = %q", got.Until)
	}
}

func TestBuild_EmptySnapshotProducesEmptySections(t *testing.T) {
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{}}
	got := Build(acc, nil)
	if len(got.InterceptionDomains) != 0 {
		t.Errorf("InterceptionDomains should be empty, got %d", len(got.InterceptionDomains))
	}
	if len(got.Hooks) != 0 {
		t.Errorf("Hooks should be empty")
	}
	if got.DiagMode != nil {
		t.Errorf("DiagMode should be nil (admin hasn't enabled)")
	}
}
