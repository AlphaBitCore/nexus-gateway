package cli

import (
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKillSwitchStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/config-sync/history" || r.URL.Query().Get("configKey") != "killswitch" {
			t.Fatalf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"events":[{"newState":{"engaged":true},"newVersion":9,"actorName":"admin"}]}`)
	}))
	defer srv.Close()

	out, err := runCLI(t, newTestApp(srv, false), "killswitch", "status")
	if err != nil || !strings.Contains(out, "engaged=true") || !strings.Contains(out, "admin") {
		t.Fatalf("killswitch status: %q err=%v", out, err)
	}
}

func TestKillSwitchStatus_JSONAndEmptyActor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"events":[{"newState":{"engaged":false},"newVersion":2,"actorName":""}]}`)
	}))
	defer srv.Close()
	// JSON output shape.
	out, err := runCLI(t, newTestApp(srv, false), "killswitch", "status", "-o", "json")
	if err != nil || !strings.Contains(out, `"Engaged": false`) {
		t.Fatalf("killswitch status json: %q err=%v", out, err)
	}
	// Table output renders an em dash for the empty actor.
	out, err = runCLI(t, newTestApp(srv, false), "killswitch", "status")
	if err != nil || !strings.Contains(out, "—") {
		t.Fatalf("empty actor should render a dash: %q err=%v", out, err)
	}
}

func TestKillSwitchAndPassthrough_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	for _, args := range [][]string{
		{"killswitch", "status"},
		{"passthrough", "status"},
		{"passthrough", "global", "on"},
	} {
		if _, err := runCLI(t, newTestApp(srv, false), args...); err == nil {
			t.Errorf("%v should surface the server error", args)
		}
	}
}

func TestPassthroughStatus_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"global":{"enabled":false},"adapters":{},"providers":{}}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "passthrough", "status", "-o", "json")
	if err != nil || !strings.Contains(out, `"global"`) {
		t.Fatalf("passthrough status json: %q err=%v", out, err)
	}
}

func TestKillSwitchStatus_NeverToggled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "killswitch", "status")
	if err != nil || !strings.Contains(out, "never toggled") {
		t.Fatalf("expected never-toggled output: %q err=%v", out, err)
	}
}

func TestPassthroughStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/passthrough/snapshot" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"global":{"enabled":true,"bypassHooks":true},"adapters":{},"providers":{"p1":{"enabled":true,"bypassHooks":true}}}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "passthrough", "status")
	if err != nil || !strings.Contains(out, "global passthrough: ON") || !strings.Contains(out, "1 provider(s)") {
		t.Fatalf("passthrough status: %q err=%v", out, err)
	}
}

func TestPassthroughGlobalOn_SetsBypassHooks(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "passthrough", "global", "on", "--reason", "incident")
	if err != nil || !strings.Contains(out, "ON") {
		t.Fatalf("passthrough global on: %q err=%v", out, err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/admin/passthrough/global" {
		t.Fatalf("wrong request: %s %s", gotMethod, gotPath)
	}
	if gotBody["enabled"] != true || gotBody["bypassHooks"] != true || gotBody["reason"] != "incident" {
		t.Fatalf("global on should send enabled+bypassHooks+reason: %+v", gotBody)
	}
}

func TestPassthroughGlobalOn_BypassFlagsRideIntoRequest(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// --bypass-cache / --bypass-normalize must add their tiers alongside the default
	// hook bypass (previously these flags had no wire-level assertion).
	if _, err := runCLI(t, newTestApp(srv, false), "passthrough", "global", "on", "--bypass-cache", "--bypass-normalize"); err != nil {
		t.Fatalf("passthrough global on with bypass flags: err=%v", err)
	}
	if gotBody["bypassHooks"] != true || gotBody["bypassCache"] != true || gotBody["bypassNormalize"] != true {
		t.Fatalf("--bypass-cache/--bypass-normalize must ride in alongside bypassHooks: %+v", gotBody)
	}
	// Without the flags only hooks are bypassed — the extra tiers stay off.
	gotBody = nil
	if _, err := runCLI(t, newTestApp(srv, false), "passthrough", "global", "on"); err != nil {
		t.Fatalf("passthrough global on (no flags): err=%v", err)
	}
	if gotBody["bypassHooks"] != true || gotBody["bypassCache"] != false || gotBody["bypassNormalize"] != false {
		t.Fatalf("without flags only hooks bypass; cache/normalize must be false: %+v", gotBody)
	}
}

func TestPassthroughGlobalProdRequiresYes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("prod global toggle without --yes must not hit the server")
	}))
	defer srv.Close()
	if _, err := runCLI(t, newTestApp(srv, true), "passthrough", "global", "on"); err == nil {
		t.Fatal("prod global passthrough without --yes should error")
	}
}

func TestPassthroughGlobalOff(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "passthrough", "global", "off")
	if err != nil || !strings.Contains(out, "OFF") {
		t.Fatalf("passthrough global off: %q err=%v", out, err)
	}
	if gotBody["enabled"] != false {
		t.Fatalf("global off should send enabled=false: %+v", gotBody)
	}
}
