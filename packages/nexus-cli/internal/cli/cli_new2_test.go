package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
)

// stubCanvas is a no-op capabilities.Canvas for wiring the conversation agent in
// tests — the agent is built but never driven, so the drives need not do anything.
type stubCanvas struct{}

func (stubCanvas) Navigate(string, core.TrafficFilter) error { return nil }
func (stubCanvas) ShowEvent(string) error                    { return nil }
func (stubCanvas) Highlight(string) error                    { return nil }

// TestTuiDeps_BuildAgentWired asserts the dashboard's conversation gets a real
// agent: tuiDeps wires BuildAgent, and invoking it returns a runnable agent built
// over the live env (no network — the agent is assembled, not run). A nil seam
// would silently disable the entire conversation sidebar, so this guards Phase 5.
func TestTuiDeps_BuildAgentWired(t *testing.T) {
	// Redirect the user config dir so memory/session/skill paths land in a temp
	// tree rather than touching the developer's real ~/.config/nexus.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	a := appWithVK(srv)

	build := a.tuiDeps().BuildAgent
	if build == nil {
		t.Fatal("tuiDeps must wire BuildAgent so the conversation sidebar has an agent")
	}
	var sawText bool
	runner, err := build(
		stubCanvas{},
		func(context.Context, agent.Tool, json.RawMessage, string) (bool, error) { return false, nil },
		func(string) { sawText = true },
		func(string) {},
		func(string, []byte) {},
		func(string, []byte, bool) {},
		func(agent.ContextStats, int) {},
		func(agent.CompactStat) {},
	)
	if err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	if runner == nil {
		t.Fatal("BuildAgent must return a runnable agent, not nil")
	}
	_ = sawText // the callback is wired into the agent; exercised by the agent loop, not here
}

// TestTuiDeps_BuildAgentConfigDirError asserts the seam surfaces a config-dir
// resolution failure (named failure mode) instead of returning a half-built agent.
func TestTuiDeps_BuildAgentConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	a := appWithVK(srv)

	runner, err := a.tuiDeps().BuildAgent(
		stubCanvas{},
		func(context.Context, agent.Tool, json.RawMessage, string) (bool, error) { return false, nil },
		func(string) {}, func(string) {}, func(string, []byte) {},
		func(string, []byte, bool) {},
		func(agent.ContextStats, int) {},
		func(agent.CompactStat) {},
	)
	if err == nil {
		t.Fatal("BuildAgent must surface a config-dir resolution failure")
	}
	if runner != nil {
		t.Fatal("a failed build must return a nil agent, not a half-built one")
	}
}

func TestLaunchTUI_Hook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	a := appWithVK(srv)
	a.Interactive = func() bool { return true }
	var launched bool
	a.LaunchTUI = func(*App) error { launched = true; return nil }
	if _, err := runCLI(t, a); err != nil || !launched {
		t.Fatalf("no-subcommand on a TTY should launch the TUI hook: launched=%v err=%v", launched, err)
	}
}

func TestTUIDeps_Closures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	dir := t.TempDir() + "/config.toml"
	cfg, err := local.Load(dir) // default config carries a writable path
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	store := fakeStore{m: map[string]string{"local:" + core.SecretAdminKey: "nxk", "local:" + core.SecretVKSecret: "nvk_stored"}}
	a := &App{
		Cfg: cfg, Env: cfg.Envs["local"], Store: store, HTTP: srv.Client(),
		BrowserOpener: func(string) error { return errors.New("no browser here") },
	}
	a.Env.LastModel = "gpt-4o-mini"

	d := a.tuiDeps()
	if d.Session.EnvName != "local" || d.Session.Model != "gpt-4o-mini" || d.Session.VKSecret != "nvk_stored" {
		t.Fatalf("session not assembled: %+v", d.Session)
	}
	if !d.HasSession() {
		t.Fatal("HasSession should be true (admin key stored)")
	}
	if err := d.SaveVKSecret("nvk_new"); err != nil {
		t.Fatalf("SaveVKSecret: %v", err)
	}
	if got, _ := store.Get("local", core.SecretVKSecret); got != "nvk_new" {
		t.Fatalf("VK secret not stored: %q", got)
	}
	if err := d.SaveSelection("m2", "vk2", "name2"); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	if a.Env.LastModel != "m2" || a.Cfg.Envs["local"].LastVKID != "vk2" {
		t.Fatalf("selection not persisted: env=%+v", a.Cfg.Envs["local"])
	}
	// Login closure reaches LoginBrowser, which fails fast via the stub opener.
	if err := d.Login(context.Background()); err == nil || !strings.Contains(err.Error(), "browser") {
		t.Fatalf("Login should surface the browser-open failure, got %v", err)
	}
	// CreateVK closure reaches the client; the empty-body stub server yields no
	// plaintext key, so it surfaces an error (exercises the wiring).
	if _, _, _, err := d.CreateVK(context.Background(), "x"); err == nil {
		t.Fatal("CreateVK closure should surface the no-plaintext-key error from the stub server")
	}
	// HasSession false when no credential is stored.
	a2 := &App{Cfg: cfg, Env: cfg.Envs["local"], Store: fakeStore{m: map[string]string{}}, HTTP: srv.Client()}
	if a2.tuiDeps().HasSession() {
		t.Fatal("HasSession should be false with no stored credential")
	}
}

func TestSimulateCmd_FlagVKAndNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body core.SimulatorForwardRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.VK != "nvk_flag" {
			t.Errorf("--vk should override the stored secret, got %q", body.VK)
		}
		_, _ = io.WriteString(w, `not-json-error-envelope`)
	}))
	defer srv.Close()
	out, err := runCLI(t, appWithVK(srv), "simulate", "--vk", "nvk_flag", "--model", "m")
	if err != nil || !strings.Contains(out, "not-json-error-envelope") {
		t.Fatalf("simulate non-JSON should print raw: %q err=%v", out, err)
	}
}

func TestSimulateCmd_UsageErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	// no model + no remembered selection → usage exit 2
	a := newTestApp(srv, false)
	_ = a.Store.Set("local", core.SecretVKSecret, "nvk_x")
	if _, err := runCLI(t, a, "simulate"); exitCode(err) != 2 {
		t.Fatalf("missing model should be exit 2, got %d", exitCode(err))
	}
	// model present but no VK secret → usage exit 2
	a2 := newTestApp(srv, false)
	a2.Env.LastModel = "m"
	if _, err := runCLI(t, a2, "simulate"); exitCode(err) != 2 {
		t.Fatalf("missing VK should be exit 2, got %d", exitCode(err))
	}
}

func TestSLOCmd_FallbackAndSparklineErrors(t *testing.T) {
	mk := func(fail string) error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, fail) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "latency-phases") {
				_, _ = io.WriteString(w, `{"window":{},"rows":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[]}`)
		}))
		defer srv.Close()
		_, err := runCLI(t, newTestApp(srv, false), "slo")
		return err
	}
	if mk("fallbacks") == nil {
		t.Fatal("slo should propagate the fallbacks error")
	}
	if mk("sparkline") == nil {
		t.Fatal("slo should propagate the sparkline error")
	}
}

func TestTUIDeps_HasSessionViaAccessToken(t *testing.T) {
	store := fakeStore{m: map[string]string{"local:" + core.SecretAccessToken: "jwt-token"}}
	a := &App{
		Cfg: &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local"}}},
		Env: core.Env{Name: "local"}, Store: store, HTTP: http.DefaultClient,
	}
	if !a.tuiDeps().HasSession() {
		t.Fatal("HasSession should be true when an access token is stored")
	}
}

func TestSLOCmd_ErrorAndNoFallbacks(t *testing.T) {
	// latency-phases 500 → command errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/analytics/latency-phases" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	if _, err := runCLI(t, newTestApp(srv, false), "slo"); err == nil {
		t.Fatal("slo should propagate the latency-phases error")
	}

	// healthy but no fallbacks + no metrics → availability 100%.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/analytics/latency-phases":
			_, _ = io.WriteString(w, `{"window":{},"rows":[]}`)
		default:
			_, _ = io.WriteString(w, `{"data":[]}`) // fallbacks + sparkline (no series)
		}
	}))
	defer srv2.Close()
	out, err := runCLI(t, newTestApp(srv2, false), "slo")
	if err != nil || !strings.Contains(out, "availability: 100.00%") {
		t.Fatalf("slo with no data should show 100%% availability: %q err=%v", out, err)
	}
}
