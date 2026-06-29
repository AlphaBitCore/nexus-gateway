package config

import (
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// fakeThingclient is a stand-in for *thingclient.Client that returns preset
// values for the /runtime/* handlers.
type fakeThingclient struct {
	desired    int64
	reported   int64
	perKey     map[string]int64
	reportedAt string
}

func (f *fakeThingclient) DesiredVer() int64  { return f.desired }
func (f *fakeThingclient) ReportedVer() int64 { return f.reported }
func (f *fakeThingclient) KeyVersion(key string) int64 {
	if f.perKey == nil {
		return 0
	}
	return f.perKey[key]
}
func (f *fakeThingclient) LastReportedAt() string { return f.reportedAt }

// fakeExemption is a canned ExemptionSnapshotter.
type fakeExemption struct{ state identity.ActiveExemptions }

func (f *fakeExemption) Snapshot() identity.ActiveExemptions { return f.state }

// fakeKillswitch is a canned KillswitchSnapshotter.
type fakeKillswitch struct{ state interception.Killswitch }

func (f *fakeKillswitch) Snapshot() interception.Killswitch { return f.state }

func (f *fakeKillswitch) ApplyBreakGlass(ks interception.Killswitch) error {
	f.state = ks
	return nil
}

// newTestRuntimeDeps assembles a RuntimeDeps with fakes for the read-surface
// snapshotters plus whatever minimum legacy fields the handlers need to not
// panic.
func newTestRuntimeDeps(t *testing.T) (RuntimeDeps, *fakeThingclient, *fakeExemption, *fakeKillswitch) {
	t.Helper()

	tc := &fakeThingclient{perKey: map[string]int64{}}
	ex := &fakeExemption{state: identity.ActiveExemptions{Entries: []identity.ActiveExemption{}}}
	ks := &fakeKillswitch{state: interception.Killswitch{Engaged: true}}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	readiness := &atomic.Bool{}
	readiness.Store(true)

	deps := RuntimeDeps{
		ThingID:        "proxy-test-1",
		ThingType:      "compliance-proxy",
		Thingclient:    tc,
		ExemptionSnap:  ex,
		KillswitchSnap: ks,
		Logger:         logger,
		Readiness:      readiness,
		StartTime:      time.Now(),
		Health: HealthChecks{Run: func(ctx context.Context) map[string]string {
			return map[string]string{"hub": "ok"}
		}},
	}
	return deps, tc, ex, ks
}

func TestRuntimeConfig_ReturnsAllKeys(t *testing.T) {
	deps, tc, ex, _ := newTestRuntimeDeps(t)
	tc.perKey = map[string]int64{
		"killswitch": 3,
		"exemptions": 5,
	}
	ex.state = identity.ActiveExemptions{Entries: []identity.ActiveExemption{{ID: "e1", SourceIP: "10.0.0.1", TargetHost: "api.openai.com"}}}

	h := HandleRuntimeConfig(deps)
	req := httptest.NewRequest(http.MethodGet, "/runtime/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ThingID     string         `json:"thingId"`
		ThingType   string         `json:"thingType"`
		Configs     map[string]any `json:"configs"`
		DesiredVer  int64          `json:"desiredVer"`
		ReportedVer int64          `json:"reportedVer"`
		InSync      bool           `json:"inSync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ThingID != "proxy-test-1" {
		t.Errorf("thingId = %q", body.ThingID)
	}
	for _, k := range []string{"exemptions", "killswitch"} {
		if _, ok := body.Configs[k]; !ok {
			t.Errorf("config %q missing from response: %+v", k, body.Configs)
		}
	}
}

func TestRuntimeConfigKey_Returns404ForUnknown(t *testing.T) {
	deps, _, _, _ := newTestRuntimeDeps(t)
	req := httptest.NewRequest(http.MethodGet, "/runtime/config/unknown", nil)
	rec := httptest.NewRecorder()
	HandleRuntimeConfigKey(deps, "unknown").ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRuntimeSyncStatus_ReturnsDesiredReported(t *testing.T) {
	deps, tc, _, _ := newTestRuntimeDeps(t)
	tc.desired = 7
	tc.reported = 7
	tc.reportedAt = "2026-04-21T00:00:00Z"

	req := httptest.NewRequest(http.MethodGet, "/runtime/sync-status", nil)
	rec := httptest.NewRecorder()
	HandleRuntimeSyncStatus(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		DesiredVer  int64  `json:"desiredVer"`
		ReportedVer int64  `json:"reportedVer"`
		InSync      bool   `json:"inSync"`
		LastSyncAt  string `json:"lastSyncAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.DesiredVer != 7 || body.ReportedVer != 7 || !body.InSync {
		t.Fatalf("unexpected body: %+v", body)
	}
	if body.LastSyncAt != "2026-04-21T00:00:00Z" {
		t.Errorf("lastSyncAt = %q", body.LastSyncAt)
	}
}

func TestRuntimeSyncStatus_NotInSync(t *testing.T) {
	deps, tc, _, _ := newTestRuntimeDeps(t)
	tc.desired = 10
	tc.reported = 8

	req := httptest.NewRequest(http.MethodGet, "/runtime/sync-status", nil)
	rec := httptest.NewRecorder()
	HandleRuntimeSyncStatus(deps).ServeHTTP(rec, req)
	var body struct {
		InSync bool `json:"inSync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.InSync {
		t.Fatalf("expected inSync=false when reported < desired")
	}
}

func TestRuntimeHealth_OverallDegradedWhenAnyFailing(t *testing.T) {
	deps, _, _, _ := newTestRuntimeDeps(t)
	deps.Health = HealthChecks{Run: func(ctx context.Context) map[string]string {
		return map[string]string{"hub": "ok", "db": "unreachable"}
	}}

	req := httptest.NewRequest(http.MethodGet, "/runtime/health", nil)
	rec := httptest.NewRecorder()
	HandleRuntimeHealth(deps, deps.Health).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "degraded" {
		t.Fatalf("expected degraded, got %q", body.Status)
	}
	if body.Checks["db"] != "unreachable" {
		t.Errorf("checks not forwarded: %+v", body.Checks)
	}
}

// TestRuntimeConfig_MethodNotAllowed covers the
// `r.Method != http.MethodGet` branch across all four runtime_config
// handlers. POST → 405 from each.
func TestRuntimeConfig_MethodNotAllowed(t *testing.T) {
	deps, _, _, _ := newTestRuntimeDeps(t)
	cases := []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{"HandleRuntimeConfig", HandleRuntimeConfig(deps), "/runtime/config"},
		{"HandleRuntimeConfigKey", HandleRuntimeConfigKey(deps, "killswitch"), "/runtime/config/killswitch"},
		{"HandleRuntimeSyncStatus", HandleRuntimeSyncStatus(deps), "/runtime/sync-status"},
		{"HandleRuntimeHealth", HandleRuntimeHealth(deps, HealthChecks{}), "/runtime/health"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			tc.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d, want 405", tc.name, rec.Code)
			}
		})
	}
}

// TestRuntimeConfigKey_KnownKey_ReturnsState covers the success branch
// in HandleRuntimeConfigKey — without it, only the 404-unknown-key
// branch was exercised.
func TestRuntimeConfigKey_KnownKey_ReturnsState(t *testing.T) {
	deps, _, _, _ := newTestRuntimeDeps(t)
	h := HandleRuntimeConfigKey(deps, "killswitch")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runtime/config/killswitch", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["key"] != "killswitch" {
		t.Errorf("key: got %v, want killswitch", body["key"])
	}
	if _, ok := body["state"]; !ok {
		t.Error("response missing state field")
	}
}

// TestRuntimeConfig_NilSnapshottersReturnEmptyState covers the
// `deps.KillswitchSnap == nil` and `deps.ExemptionSnap == nil`
// branches in snapshotFor — when wiring is missing, callers see
// safe-empty configtypes values instead of nil.
func TestRuntimeConfig_NilSnapshottersReturnEmptyState(t *testing.T) {
	deps := RuntimeDeps{
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StartTime: time.Now(),
	}
	// Both snapshotters nil. HandleRuntimeConfig should return 200
	// with safe-empty state for each known key.
	h := HandleRuntimeConfig(deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runtime/config", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	configs, ok := body["configs"].(map[string]any)
	if !ok {
		t.Fatal("configs missing")
	}
	if _, ok := configs["killswitch"]; !ok {
		t.Error("killswitch key absent")
	}
	if _, ok := configs["exemptions"]; !ok {
		t.Error("exemptions key absent")
	}
}

// TestRuntimeHealth_NilCheckRunReturnsEmptyChecks covers the
// `if checks.Run != nil` false branch — when no probe is wired the
// handler must still return 200 + status:ok + empty checks map.
func TestRuntimeHealth_NilCheckRunReturnsEmptyChecks(t *testing.T) {
	deps, _, _, _ := newTestRuntimeDeps(t)
	h := HandleRuntimeHealth(deps, HealthChecks{Run: nil})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runtime/health", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" {
		t.Errorf("status: got %q, want ok", body.Status)
	}
	if len(body.Checks) != 0 {
		t.Errorf("checks: got %+v, want empty", body.Checks)
	}
}

// TestRuntimeHealth_RunReturnsNilTreatedAsEmpty covers the
// `if out != nil` false branch — a probe that returns nil should be
// treated as "no checks reporting", same as no probe wired.
func TestRuntimeHealth_RunReturnsNilTreatedAsEmpty(t *testing.T) {
	deps, _, _, _ := newTestRuntimeDeps(t)
	h := HandleRuntimeHealth(deps, HealthChecks{Run: func(_ context.Context) map[string]string {
		return nil
	}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runtime/health", nil)
	h.ServeHTTP(rec, req)
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" {
		t.Errorf("nil checks should treat as ok: %q", body.Status)
	}
}
