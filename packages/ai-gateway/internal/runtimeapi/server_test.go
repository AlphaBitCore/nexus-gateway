package runtimeapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

type fakeThingClient struct {
	desired      map[string]thingclient.ConfigState
	desiredVer   int64
	reportedVer  int64
	lastReported string
}

func (f *fakeThingClient) SnapshotDesired() map[string]thingclient.ConfigState { return f.desired }
func (f *fakeThingClient) DesiredVer() int64                                   { return f.desiredVer }
func (f *fakeThingClient) ReportedVer() int64                                  { return f.reportedVer }
func (f *fakeThingClient) LastReportedAt() string                              { return f.lastReported }
func (f *fakeThingClient) KeyVersion(k string) int64 {
	if cs, ok := f.desired[k]; ok {
		return cs.Version
	}
	return 0
}

func TestRuntimeConfig_ListsAllKeys(t *testing.T) {
	tc := &fakeThingClient{
		desired: map[string]thingclient.ConfigState{
			"observability": {State: json.RawMessage(`{"log_level":"info"}`), Version: 3},
			"hooks":         {State: json.RawMessage(`{"hooks":[]}`), Version: 7},
		},
		desiredVer: 7, reportedVer: 7, lastReported: "2026-04-20T10:00:00Z",
	}
	srv := New(Config{APIToken: "api", Thing: tc})

	req := httptest.NewRequest(http.MethodGet, "/runtime/config", nil)
	req.Header.Set("Authorization", "Bearer api")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	m, _ := out["config"].(map[string]any)
	if len(m) != 2 {
		t.Fatalf("want 2 keys, got %d", len(m))
	}
}

func TestRuntimeConfigKey_ReturnsSingleEntry(t *testing.T) {
	tc := &fakeThingClient{
		desired: map[string]thingclient.ConfigState{
			"observability": {State: json.RawMessage(`{"log_level":"debug"}`), Version: 5},
		},
		desiredVer: 5, reportedVer: 5,
	}
	srv := New(Config{APIToken: "api", Thing: tc})

	req := httptest.NewRequest(http.MethodGet, "/runtime/config/observability", nil)
	req.Header.Set("Authorization", "Bearer api")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if v, _ := entry["version"].(float64); int64(v) != 5 {
		t.Fatalf("want version 5, got %v", entry["version"])
	}
}

func TestRuntimeConfigKey_UnknownReturns404(t *testing.T) {
	tc := &fakeThingClient{desired: map[string]thingclient.ConfigState{}}
	srv := New(Config{APIToken: "api", Thing: tc})

	req := httptest.NewRequest(http.MethodGet, "/runtime/config/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer api")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestRuntimeSyncStatus_ReportsDrift(t *testing.T) {
	tc := &fakeThingClient{desiredVer: 10, reportedVer: 9, lastReported: "2026-04-20T10:00:00Z"}
	srv := New(Config{APIToken: "api", Thing: tc})
	req := httptest.NewRequest(http.MethodGet, "/runtime/sync-status", nil)
	req.Header.Set("Authorization", "Bearer api")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["in_sync"] != false {
		t.Fatalf("want in_sync=false, got %v", out["in_sync"])
	}
}

func TestRuntimeHealth_OK(t *testing.T) {
	tc := &fakeThingClient{desiredVer: 1, reportedVer: 1}
	srv := New(Config{APIToken: "api", Thing: tc})
	req := httptest.NewRequest(http.MethodGet, "/runtime/health", nil)
	req.Header.Set("Authorization", "Bearer api")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

// TestMount_RegistersAllGETRoutes verifies Mount wires the four read routes
// onto an external mux so main.go can host them alongside /v1 traffic on the
// same ServeMux.
func TestNew_PanicsOnNilThing(t *testing.T) {
	// Constructor contract: nil Thing is a programmer bug surfaced at
	// startup, not a runtime no-op. Without this guard the auth-check
	// handler would later nil-deref serving traffic.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New with nil Thing must panic")
		}
	}()
	_ = New(Config{APIToken: "t"})
}

func TestNew_DefaultLoggerWhenNil(t *testing.T) {
	// Nil logger must default to slog.Default — without it, every handler
	// call would nil-deref s.logger.
	tc := &fakeThingClient{desired: map[string]thingclient.ConfigState{}}
	srv := New(Config{Thing: tc}) // logger nil
	if srv.logger == nil {
		t.Error("New should default Logger to slog.Default when nil")
	}
}

func TestMount_RegistersAllGETRoutes(t *testing.T) {
	tc := &fakeThingClient{
		desired: map[string]thingclient.ConfigState{
			"observability": {State: json.RawMessage(`{"log_level":"info"}`), Version: 1},
		},
		desiredVer: 1, reportedVer: 1,
	}
	srv := New(Config{APIToken: "api", Thing: tc})

	parent := http.NewServeMux()
	srv.Mount(parent)

	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/runtime/config"},
		{"GET", "/runtime/config/observability"},
		{"GET", "/runtime/sync-status"},
		{"GET", "/runtime/health"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer api")
		w := httptest.NewRecorder()
		parent.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: want 200, got %d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
