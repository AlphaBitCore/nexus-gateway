package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
)

func TestMain_Entry(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()

	os.Args = []string{"nexus", "--help"}
	if code := Main(); code != 0 {
		t.Fatalf("--help exit = %d, want 0", code)
	}
	os.Args = []string{"nexus", "env", "use"} // missing arg → usage
	if code := Main(); code != 2 {
		t.Fatalf("env use no-arg exit = %d, want 2", code)
	}
}

func TestHealth_FromDiskConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/analytics/sparkline":
			_, _ = io.WriteString(w, `{"granularity":"1h","summary":{},"series":[]}`)
		case "/api/admin/instances":
			_, _ = io.WriteString(w, `{"count":9,"services":{"nexus-hub":{"total":2}}}`)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := "default_env = \"local\"\n[envs.local]\nname = \"local\"\ncp_base_url = \"" + srv.URL + "\"\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{ConfigPath: path, Store: fakeStore{m: map[string]string{"local:" + core.SecretAdminKey: "nxk"}}}
	out, err := runCLI(t, a, "health", "-o", "json")
	if err != nil || !strings.Contains(out, `"nodes": 9`) {
		t.Fatalf("health from disk config wrong: %q err=%v", out, err)
	}
}

func TestLogin_Browser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_ = r.ParseForm()
			if r.Form.Get("code") != "cb-code" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"tok-abc","refresh_token":"r"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	store := fakeStore{m: map[string]string{}}
	a := &App{
		Cfg:   &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local"}}},
		Env:   core.Env{Name: "local", CPBaseURL: srv.URL, OAuthClientID: "cp-ui"},
		Store: store,
		BrowserOpener: func(authURL string) error {
			u, _ := url.Parse(authURL)
			resp, err := http.Get(u.Query().Get("redirect_uri") + "?code=cb-code&state=" + u.Query().Get("state"))
			if err == nil {
				resp.Body.Close()
			}
			return err
		},
	}
	if out, err := runCLI(t, a, "login"); err != nil || !strings.Contains(out, "Logged in") {
		t.Fatalf("browser login: %q err=%v", out, err)
	}
	if got, _ := store.Get("local", core.SecretAccessToken); got != "tok-abc" {
		t.Fatalf("token not stored: %q", got)
	}
}

func TestLogin_AdminKey_Errors(t *testing.T) {
	mk := func(stdin string, setErr error) error {
		a := &App{
			Cfg:    &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local"}}},
			Env:    core.Env{Name: "local"},
			Store:  fakeStore{m: map[string]string{}, setErr: setErr},
			Out:    io.Discard,
			ErrOut: io.Discard,
		}
		root := NewRootCmd(a)
		root.SetArgs([]string{"login", "--admin-key"})
		root.SetIn(strings.NewReader(stdin))
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		return root.Execute()
	}
	if err := mk("\n", nil); exitCode(err) != 2 {
		t.Errorf("empty key → exit %d, want 2", exitCode(err))
	}
	if err := mk("", nil); exitCode(err) != 2 { // no stdin line at all
		t.Errorf("no-stdin key → exit %d, want 2", exitCode(err))
	}
	if err := mk("nxk_ok\n", errors.New("keychain locked")); err == nil {
		t.Error("store error should propagate")
	}
}

func TestLogin_BrowserError(t *testing.T) {
	a := &App{
		Cfg:           &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local"}}},
		Env:           core.Env{Name: "local", CPBaseURL: "http://unused", OAuthClientID: "cp-ui"},
		Store:         fakeStore{m: map[string]string{}},
		BrowserOpener: func(string) error { return errors.New("no browser here") },
	}
	if _, err := runCLI(t, a, "login"); err == nil {
		t.Fatal("browser login should fail when the opener errors")
	}
}

func TestCommands_ErrorPropagation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom","code":"X"}}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	cmds := [][]string{
		{"models", "ls"},
		{"traffic", "ls"},
		{"traffic", "get", "id"},
		{"cost"},
		{"killswitch", "on"},
		{"health"},
	}
	for _, args := range cmds {
		if _, err := runCLI(t, newTestApp(srv, false), args...); !errors.Is(err, core.ErrTransport) {
			t.Errorf("%v: want ErrTransport, got %v", args, err)
		}
	}
}

func TestTrafficLs_SinceAndJSONGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/traffic":
			if r.URL.Query().Get("startTime") == "" {
				t.Error("--since should set startTime")
			}
			_, _ = io.WriteString(w, `{"data":[],"total":0,"limit":50,"offset":0}`)
		default: // /api/admin/traffic/ev1
			_, _ = io.WriteString(w, `{"id":"ev1","statusCode":200}`)
		}
	}))
	defer srv.Close()
	if _, err := runCLI(t, newTestApp(srv, false), "traffic", "ls", "--since", "1h"); err != nil {
		t.Fatalf("traffic ls --since: %v", err)
	}
	out, err := runCLI(t, newTestApp(srv, false), "traffic", "get", "ev1", "-o", "json")
	if err != nil || !strings.Contains(out, `"id": "ev1"`) {
		t.Fatalf("traffic get json: %q err=%v", out, err)
	}
}

func TestKillSwitch_OffJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"engaged":false,"version":4,"thingsNotified":1,"thingsOnline":1}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "killswitch", "off", "-o", "json")
	if err != nil || !strings.Contains(out, `"engaged": false`) {
		t.Fatalf("killswitch off json: %q err=%v", out, err)
	}
}

func TestEnsureEnv_UnknownEnvErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("default_env = \"local\"\n[envs.local]\nname=\"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{ConfigPath: path, Store: fakeStore{m: map[string]string{}}}
	// --env names an environment that isn't defined → resolution error.
	if _, err := runCLI(t, a, "health", "--env", "ghost"); err == nil {
		t.Fatal("unknown --env should error during ensureEnv")
	}
}

func TestClient_PresetIsReused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"count":1,"services":{}}`)
	}))
	defer srv.Close()
	preset := core.NewClient(core.Env{Name: "local", CPBaseURL: srv.URL},
		core.NewTokenSource(core.Env{Name: "local"}, fakeStore{m: map[string]string{"local:" + core.SecretAdminKey: "k"}}, srv.Client()),
		srv.Client())
	a := newTestApp(srv, false)
	a.Client = preset // a.client() must return this without rebuilding
	if a.client() != preset {
		t.Fatal("preset client should be reused")
	}
}

func TestEnsureConfig_DefaultPath(t *testing.T) {
	// Point HOME at a temp dir so DefaultConfigPath resolves there (no real
	// config touched); the file is absent so Load returns built-in defaults.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := &App{Store: fakeStore{m: map[string]string{}}} // no ConfigPath → DefaultConfigPath
	out, err := runCLI(t, a, "env", "ls", "-o", "json")
	if err != nil || !strings.Contains(out, `"name": "local"`) {
		t.Fatalf("env ls json from default path wrong: %q err=%v", out, err)
	}
}

func TestEnvUse_SaveError(t *testing.T) {
	// A regular file in the config path's parent makes Save's MkdirAll fail.
	fileAsDir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{ConfigPath: filepath.Join(fileAsDir, "sub", "config.toml"), Store: fakeStore{m: map[string]string{}}}
	if _, err := runCLI(t, a, "env", "use", "local"); err == nil {
		t.Fatal("env use should fail when config cannot be saved")
	}
}

func TestRoot_InteractiveLaunchesTUI(t *testing.T) {
	launched := false
	a := &App{
		Cfg:         &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local"}}},
		Env:         core.Env{Name: "local"},
		Store:       fakeStore{m: map[string]string{}},
		Interactive: func() bool { return true },
		LaunchTUI:   func(*App) error { launched = true; return nil },
	}
	if _, err := runCLI(t, a); err != nil {
		t.Fatalf("interactive root: %v", err)
	}
	if !launched {
		t.Fatal("interactive no-subcommand invocation should launch the TUI")
	}
}

func TestWithBrowserOpener_NilKeepsDefault(t *testing.T) {
	a := core.NewAuthenticator(core.Env{Name: "x"}, fakeStore{m: map[string]string{}}, http.DefaultClient)
	if a.WithBrowserOpener(nil) != a {
		t.Fatal("WithBrowserOpener(nil) should return the same authenticator")
	}
}
