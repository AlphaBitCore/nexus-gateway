package status

import (
	"encoding/json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// fakeThing is a minimal ThingStateAccessor used to drive BuildConfigSummary
// without spinning up a real thingclient.Client. Each accessor returns the
// corresponding field verbatim.
type fakeThing struct {
	desiredVer  int64
	reportedVer int64
	last        string
	desired     map[string]thingclient.ConfigState
}

func (f *fakeThing) SnapshotDesired() map[string]thingclient.ConfigState {
	return f.desired
}
func (f *fakeThing) DesiredVer() int64      { return f.desiredVer }
func (f *fakeThing) ReportedVer() int64     { return f.reportedVer }
func (f *fakeThing) LastReportedAt() string { return f.last }

// TestBuildConfigSummary_InSync_LiveShape exercises the wire shapes the
// agent actually sees today — "hookConfigs" / "interceptionDomains" with
// per-entry enabled flags — which are produced by Hub's Cat B aggregator
// and consumed by AgentPipeline.Apply{Hooks,Domains}ShadowState. The
// older "hooks" / "domains" envelopes are kept for back-compat by the
// parser and covered separately in TestBuildConfigSummary_InSync_LegacyShape.
func TestBuildConfigSummary_InSync_LiveShape(t *testing.T) {
	tc := &fakeThing{
		desiredVer:  7,
		reportedVer: 7,
		last:        "2026-04-20T10:00:00Z",
		desired: map[string]thingclient.ConfigState{
			"hooks": {
				State:   json.RawMessage(`{"hookConfigs":[{"id":"h1","enabled":true},{"id":"h2","enabled":true},{"id":"h3","enabled":false}]}`),
				Version: 7,
			},
			"interception_domains": {
				State:   json.RawMessage(`{"interceptionDomains":[{"id":"d1","enabled":true},{"id":"d2","enabled":true},{"id":"d3","enabled":false},{"id":"d4","enabled":true}]}`),
				Version: 6,
			},
			"exemptions": {
				State:   json.RawMessage(`{"active":[{"id":"e1"}]}`),
				Version: 7,
			},
		},
	}
	s := BuildConfigSummary(tc, nil)
	if s.ReportedVersion != 7 || s.ThingVersion != 7 {
		t.Fatalf("want 7/7, got thing=%d reported=%d", s.ThingVersion, s.ReportedVersion)
	}
	if !s.InSync {
		t.Fatal("want in_sync=true")
	}
	if s.LastReportedAt != "2026-04-20T10:00:00Z" {
		t.Errorf("want last=2026-04-20T10:00:00Z, got %q", s.LastReportedAt)
	}
	if s.HooksEnabled != 2 {
		t.Errorf("want 2 hooks enabled, got %d", s.HooksEnabled)
	}
	if s.InterceptionDomains != 3 {
		t.Errorf("want 3 enabled domains, got %d", s.InterceptionDomains)
	}
	if s.ActiveExemptions != 1 {
		t.Errorf("want 1 exemption, got %d", s.ActiveExemptions)
	}
}

// TestBuildConfigSummary_InSync_LegacyShape verifies the older wire
// shapes ("hooks" / "domains") still produce sensible counts so a
// rolling upgrade where Hub publishes the old envelope to an agent
// running the new parser does not visually wipe the menu-bar config
// panel.
func TestBuildConfigSummary_InSync_LegacyShape(t *testing.T) {
	tc := &fakeThing{
		desiredVer:  7,
		reportedVer: 7,
		desired: map[string]thingclient.ConfigState{
			"hooks": {
				State: json.RawMessage(`{"hooks":[{"id":"h1","enabled":true},{"id":"h2","enabled":false}]}`),
			},
			"interception_domains": {
				State: json.RawMessage(`{"domains":["a.com","b.com","c.com"]}`),
			},
		},
	}
	s := BuildConfigSummary(tc, nil)
	if s.HooksEnabled != 1 {
		t.Errorf("want 1 hook enabled (legacy shape), got %d", s.HooksEnabled)
	}
	if s.InterceptionDomains != 3 {
		t.Errorf("want 3 domains (legacy string list), got %d", s.InterceptionDomains)
	}
}

// TestBuildConfigSummary_InterceptionDomain_DefaultEnabled covers the case
// where Hub omits the per-entry "enabled" field entirely. The parser
// must treat each entry as enabled (match the admin-UI default) rather
// than dropping all of them and surfacing 0.
func TestBuildConfigSummary_InterceptionDomain_DefaultEnabled(t *testing.T) {
	tc := &fakeThing{
		desiredVer:  1,
		reportedVer: 1,
		desired: map[string]thingclient.ConfigState{
			"interception_domains": {
				State: json.RawMessage(`{"interceptionDomains":[{"id":"d1"},{"id":"d2"}]}`),
			},
		},
	}
	s := BuildConfigSummary(tc, nil)
	if s.InterceptionDomains != 2 {
		t.Errorf("want 2 domains when enabled flag omitted, got %d", s.InterceptionDomains)
	}
}

func TestBuildConfigSummary_OutOfSync(t *testing.T) {
	tc := &fakeThing{
		desiredVer:  10,
		reportedVer: 9,
		last:        "2026-04-20T09:00:00Z",
	}
	s := BuildConfigSummary(tc, nil)
	if s.InSync {
		t.Fatal("want in_sync=false")
	}
	if s.ThingVersion != 10 || s.ReportedVersion != 9 {
		t.Fatalf("want 10/9, got %d/%d", s.ThingVersion, s.ReportedVersion)
	}
	// Empty snapshot means all category counts stay at zero.
	if s.HooksEnabled != 0 || s.InterceptionDomains != 0 || s.ActiveExemptions != 0 {
		t.Errorf("want zero counts on empty snapshot, got hooks=%d domains=%d exemptions=%d",
			s.HooksEnabled, s.InterceptionDomains, s.ActiveExemptions)
	}
}

func TestBuildConfigSummary_MalformedPayloadsZeroCounts(t *testing.T) {
	// A corrupt or unrecognized payload must not panic — the builder should
	// treat it as a zero count and move on. Runs through every parsing branch.
	tc := &fakeThing{
		desiredVer:  3,
		reportedVer: 3,
		desired: map[string]thingclient.ConfigState{
			"hooks":                {State: json.RawMessage(`not-json`)},
			"interception_domains": {State: json.RawMessage(`{"domains":"wrong-shape"}`)},
			"exemptions":           {State: json.RawMessage(`{}`)},
		},
	}
	s := BuildConfigSummary(tc, nil)
	if s.HooksEnabled != 0 || s.InterceptionDomains != 0 || s.ActiveExemptions != 0 {
		t.Errorf("malformed payloads should yield zero counts, got %+v", s)
	}
}

func TestBuildConfigSummary_NilAccessor(t *testing.T) {
	// Callers may pass a nil accessor when the thingclient is not configured
	// (e.g. agent running without Hub URL). Must return a zero-value summary
	// rather than panicking — the JSON shape stays stable for the UI.
	s := BuildConfigSummary(nil, nil)
	zero := ConfigSummary{}
	// Comparing the structs directly fails because the new slice
	// fields ([]ExemptionBrief / []RulePackBrief) aren't comparable.
	// Fall back to a JSON-equality check (catches every field the UI
	// would observe, future-proof against new fields).
	gotJSON, _ := json.Marshal(s)
	wantJSON, _ := json.Marshal(zero)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("nil accessor should return zero-value\n  got:  %s\n  want: %s", gotJSON, wantJSON)
	}
}

func TestConfigSummary_JSONShape(t *testing.T) {
	s := ConfigSummary{
		ThingVersion:        5,
		ReportedVersion:     5,
		InSync:              true,
		LastReportedAt:      "2026-04-20T10:00:00Z",
		HooksEnabled:        2,
		InterceptionDomains: 3,
		ActiveExemptions:    1,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"thingVersion":5,"reportedVersion":5,"inSync":true,"lastReportedAt":"2026-04-20T10:00:00Z","hooksEnabled":2,"interceptionDomains":3,"activeExemptions":1,"exemptions":null,"rulePacks":null,"killSwitch":{"engaged":false}}`
	if got != want {
		t.Errorf("JSON shape drift:\ngot:  %s\nwant: %s", got, want)
	}
}
