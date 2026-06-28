package cli

import (
	"bytes"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
)

// fakeStore is an in-memory SecretStore for CLI tests. setErr, when non-nil,
// makes Set fail.
type fakeStore struct {
	m      map[string]string
	setErr error
}

func (s fakeStore) Get(env, key string) (string, error) {
	if v, ok := s.m[env+":"+key]; ok {
		return v, nil
	}
	return "", core.ErrSecretNotFound
}
func (s fakeStore) Set(env, key, val string) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.m[env+":"+key] = val
	return nil
}
func (s fakeStore) Delete(env, key string) error { delete(s.m, env+":"+key); return nil }

// newTestApp wires an App whose client talks to srv, authenticated by a stored
// admin key (so no JWT/login is needed). isProd marks the env prod.
func newTestApp(srv *httptest.Server, isProd bool) *App {
	store := fakeStore{m: map[string]string{"local:" + core.SecretAdminKey: "nxk_test"}}
	return &App{
		Cfg:   &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local", CPBaseURL: srv.URL, IsProd: isProd}}},
		Env:   core.Env{Name: "local", CPBaseURL: srv.URL, IsProd: isProd},
		Store: store,
		HTTP:  srv.Client(),
	}
}

// runCLI executes the root command with args, returning captured stdout.
func runCLI(t *testing.T, a *App, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	a.Out = &buf
	a.ErrOut = io.Discard
	root := NewRootCmd(a)
	root.SetArgs(args)
	root.SetOut(&buf) // capture cobra's own output (help/usage) too
	root.SetErr(io.Discard)
	err := root.Execute()
	return buf.String(), err
}

func TestModelsLs_JSONAndTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"provider":{"id":"p1","name":"OpenAI"},"models":[{"code":"gpt-4","name":"GPT-4","type":"chat","enabled":true}]}]}`)
	}))
	defer srv.Close()

	out, err := runCLI(t, newTestApp(srv, false), "models", "ls", "--output", "json")
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var cat core.ModelCatalog
	if json.Unmarshal([]byte(out), &cat); cat.Data[0].Models[0].Code != "gpt-4" {
		t.Fatalf("json output wrong: %s", out)
	}

	out, err = runCLI(t, newTestApp(srv, false), "models", "ls")
	if err != nil || !strings.Contains(out, "gpt-4") || !strings.Contains(out, "OpenAI") || !strings.Contains(out, "CODE") {
		t.Fatalf("table output wrong: %q err=%v", out, err)
	}
}

func TestTrafficLs_TableAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("statusRange") != "5xx" {
			t.Errorf("status filter not passed: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"ev1","statusCode":500,"modelName":"gpt-4","totalTokens":10,"estimatedCostUsd":0.01}],"total":1,"limit":50,"offset":0}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "traffic", "ls", "--status", "5xx")
	if err != nil || !strings.Contains(out, "ev1") || !strings.Contains(out, "1 of 1 events") {
		t.Fatalf("traffic ls table wrong: %q err=%v", out, err)
	}
	out, _ = runCLI(t, newTestApp(srv, false), "traffic", "ls", "--status", "5xx", "-o", "json")
	if !strings.Contains(out, `"total": 1`) {
		t.Fatalf("traffic ls json wrong: %s", out)
	}
}

func TestTrafficGet_AndNormalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/normalized") {
			_, _ = io.WriteString(w, `{"kind":"ai-chat"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"ev1","statusCode":200,"modelName":"gpt-4","traceId":"tr-1","totalTokens":42}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "traffic", "get", "ev1")
	if err != nil || !strings.Contains(out, "tr-1") || !strings.Contains(out, "ev1") {
		t.Fatalf("traffic get wrong: %q err=%v", out, err)
	}
	out, _ = runCLI(t, newTestApp(srv, false), "traffic", "get", "ev1", "--normalized")
	if !strings.Contains(out, "ai-chat") {
		t.Fatalf("normalized wrong: %s", out)
	}
	// missing arg → usage error
	if _, err := runCLI(t, newTestApp(srv, false), "traffic", "get"); exitCode(err) != 2 {
		t.Fatalf("missing arg should be usage exit 2, got %d (%v)", exitCode(err), err)
	}
}

func TestHealth_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/analytics/sparkline":
			_, _ = io.WriteString(w, `{"granularity":"1h","summary":{"requestCount":5},"series":[]}`)
		case "/api/admin/instances":
			_, _ = io.WriteString(w, `{"count":27,"services":{"ai-gateway":{"total":3}}}`)
		}
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "health", "-o", "json")
	if err != nil || !strings.Contains(out, `"nodes": 27`) || !strings.Contains(out, "ai-gateway") {
		t.Fatalf("health json wrong: %q err=%v", out, err)
	}
	out, _ = runCLI(t, newTestApp(srv, false), "health")
	if !strings.Contains(out, "ai-gateway") || !strings.Contains(out, "nodes:") {
		t.Fatalf("health table wrong: %s", out)
	}
}

func TestCost_TableAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"group":"p1","groupLabel":"OpenAI","requestCount":111,"totalTokens":1000,"totalCostUsd":1.5,"cacheHitCount":2}],"total":1}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "cost", "--group", "provider")
	if err != nil || !strings.Contains(out, "OpenAI") || !strings.Contains(out, "total cost: 1.5000") {
		t.Fatalf("cost table wrong: %q err=%v", out, err)
	}
	out, _ = runCLI(t, newTestApp(srv, false), "cost", "-o", "json")
	if !strings.Contains(out, `"totalCostUsd": 1.5`) {
		t.Fatalf("cost json wrong: %s", out)
	}
}

func TestKillSwitch_OnAndProdGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body["engaged"] {
			t.Error("expected engaged=true")
		}
		_, _ = io.WriteString(w, `{"engaged":true,"version":3,"thingsNotified":2,"thingsOnline":2}`)
	}))
	defer srv.Close()
	// non-prod: on works without --yes.
	out, err := runCLI(t, newTestApp(srv, false), "killswitch", "on")
	if err != nil || !strings.Contains(out, "engaged=true") {
		t.Fatalf("killswitch on wrong: %q err=%v", out, err)
	}
	// prod: requires --yes.
	if _, err := runCLI(t, newTestApp(srv, true), "killswitch", "on"); exitCode(err) != 2 {
		t.Fatalf("prod without --yes should be usage exit 2, got %d (%v)", exitCode(err), err)
	}
	// prod with --yes works.
	out, err = runCLI(t, newTestApp(srv, true), "killswitch", "on", "--yes")
	if err != nil || !strings.Contains(out, "engaged=true") {
		t.Fatalf("prod killswitch --yes wrong: %q err=%v", out, err)
	}
}

func TestExitCode_Mapping(t *testing.T) {
	mk := func(status int) error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"x","code":"C"}}`)
		}))
		defer srv.Close()
		_, err := runCLI(t, newTestApp(srv, false), "health")
		return err
	}
	if c := exitCode(mk(401)); c != 3 {
		t.Errorf("401 → exit %d, want 3", c)
	}
	if c := exitCode(mk(403)); c != 4 {
		t.Errorf("403 → exit %d, want 4", c)
	}
	if c := exitCode(mk(404)); c != 5 {
		t.Errorf("404 → exit %d, want 5", c)
	}
	if c := exitCode(mk(500)); c != 1 {
		t.Errorf("500 → exit %d, want 1", c)
	}
	if exitCode(nil) != 0 {
		t.Error("nil → exit 0")
	}
}

func TestEnvLsAndUse(t *testing.T) {
	dir := t.TempDir() + "/config.toml"
	a := &App{ConfigPath: dir, Store: fakeStore{m: map[string]string{}}}
	// ls shows the built-in local default.
	out, err := runCLI(t, a, "env", "ls")
	if err != nil || !strings.Contains(out, "local") || !strings.Contains(out, "*") {
		t.Fatalf("env ls wrong: %q err=%v", out, err)
	}
	// use an undefined env → error.
	a2 := &App{ConfigPath: dir, Store: fakeStore{m: map[string]string{}}}
	if _, err := runCLI(t, a2, "env", "use", "ghost"); err == nil {
		t.Fatal("env use ghost should error")
	}
	// use the local env → persists.
	a3 := &App{ConfigPath: dir, Store: fakeStore{m: map[string]string{}}}
	if out, err := runCLI(t, a3, "env", "use", "local"); err != nil || !strings.Contains(out, "local") {
		t.Fatalf("env use local: %q err=%v", out, err)
	}
}

func TestLogin_AdminKeyFromStdin(t *testing.T) {
	store := fakeStore{m: map[string]string{}}
	a := &App{
		Cfg:    &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local"}}},
		Env:    core.Env{Name: "local"},
		Store:  store,
		Out:    io.Discard,
		ErrOut: io.Discard,
	}
	root := NewRootCmd(a)
	root.SetArgs([]string{"login", "--admin-key"})
	root.SetIn(strings.NewReader("nxk_pasted_key\n"))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("login --admin-key: %v", err)
	}
	if got, _ := store.Get("local", core.SecretAdminKey); got != "nxk_pasted_key" {
		t.Fatalf("admin key not stored: %q", got)
	}
}

func TestRoot_NoArgsShowsHelp(t *testing.T) {
	a := &App{
		Cfg: &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local"}}},
		Env: core.Env{Name: "local"}, Store: fakeStore{m: map[string]string{}},
	}
	out, err := runCLI(t, a)
	if err != nil || !strings.Contains(out, "Operate and observe") {
		t.Fatalf("root help wrong: %q err=%v", out, err)
	}
}
