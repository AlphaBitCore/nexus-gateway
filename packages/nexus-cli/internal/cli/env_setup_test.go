package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// newConfigApp builds an App backed by a temp config file (no network), so the
// env-management commands persist to disk without touching the real config.
func newConfigApp(t *testing.T) *App {
	t.Helper()
	return &App{
		Store:      fakeStore{m: map[string]string{}},
		ConfigPath: filepath.Join(t.TempDir(), "config.toml"),
	}
}

// runCLIWithIn executes the root command feeding stdin (for interactive setup).
func runCLIWithIn(t *testing.T, a *App, stdin string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	a.Out = &buf
	a.ErrOut = io.Discard
	root := NewRootCmd(a)
	root.SetArgs(args)
	root.SetOut(&buf)
	root.SetErr(io.Discard)
	root.SetIn(strings.NewReader(stdin))
	err := root.Execute()
	return buf.String(), err
}

func TestSetup_CreatesEnvAndSetsDefault(t *testing.T) {
	a := newConfigApp(t)
	in := "dev\nhttps://cp.example.com\nhttps://aigw.example.com\ncp-ui\nhttp://localhost:3000/auth/callback\ny\n"
	out, err := runCLIWithIn(t, a, in, "setup")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(out, `Saved environment "dev"`) || !strings.Contains(out, "nexus login") {
		t.Fatalf("setup should confirm + guide to login:\n%s", out)
	}
	cfg, err := core.Load(a.ConfigPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	e, ok := cfg.Envs["dev"]
	if !ok || e.CPBaseURL != "https://cp.example.com" || e.AIGatewayBaseURL != "https://aigw.example.com" || !e.IsProd {
		t.Fatalf("setup persisted wrong env: %+v", e)
	}
	if cfg.DefaultEnv != "dev" {
		t.Fatalf("setup should set the new env as default, got %q", cfg.DefaultEnv)
	}
}

func TestSetup_RequiresCPURL(t *testing.T) {
	a := newConfigApp(t)
	// name "dev", then an empty Control Plane URL (no current value to default to).
	_, err := runCLIWithIn(t, a, "dev\n\n", "setup")
	if err == nil || exitCode(err) != 2 {
		t.Fatalf("empty CP URL should be a usage error (exit 2), got %v (code %d)", err, exitCode(err))
	}
}

func TestEnvAdd_DefaultsAndRm(t *testing.T) {
	a := newConfigApp(t)
	if _, err := runCLIWithIn(t, a, "", "env", "add", "prod", "--cp-url", "https://prod.example.com", "--prod"); err != nil {
		t.Fatalf("env add: %v", err)
	}
	cfg, _ := core.Load(a.ConfigPath)
	e := cfg.Envs["prod"]
	if e.CPBaseURL != "https://prod.example.com" || e.AIGatewayBaseURL != "https://prod.example.com" || e.OAuthClientID != "cp-ui" || !e.IsProd {
		t.Fatalf("env add defaults wrong: %+v", e)
	}
	// env add does not hijack an existing default (built-in local seeds it);
	// the operator switches with `env use`.
	if cfg.DefaultEnv != "local" {
		t.Fatalf("env add should leave the existing default, got %q", cfg.DefaultEnv)
	}
	a5 := &App{Store: fakeStore{m: map[string]string{}}, ConfigPath: a.ConfigPath}
	if _, err := runCLIWithIn(t, a5, "", "env", "use", "prod"); err != nil {
		t.Fatalf("env use: %v", err)
	}
	cfg, _ = core.Load(a.ConfigPath)
	if cfg.DefaultEnv != "prod" {
		t.Fatalf("env use should switch the default, got %q", cfg.DefaultEnv)
	}

	// add without --cp-url is a usage error.
	a2 := newConfigApp(t)
	if _, err := runCLIWithIn(t, a2, "", "env", "add", "x"); err == nil || exitCode(err) != 2 {
		t.Fatalf("env add without --cp-url should be usage error, got %v", err)
	}

	// rm removes it; rm of an unknown env errors.
	a3 := &App{Store: fakeStore{m: map[string]string{}}, ConfigPath: a.ConfigPath}
	if _, err := runCLIWithIn(t, a3, "", "env", "rm", "prod"); err != nil {
		t.Fatalf("env rm: %v", err)
	}
	cfg, _ = core.Load(a.ConfigPath)
	if _, ok := cfg.Envs["prod"]; ok {
		t.Fatal("env rm should delete the env")
	}
	a4 := &App{Store: fakeStore{m: map[string]string{}}, ConfigPath: a.ConfigPath}
	if _, err := runCLIWithIn(t, a4, "", "env", "rm", "ghost"); err == nil {
		t.Fatal("env rm of unknown env should error")
	}
}

func TestGuard_NotLoggedInGuidesToLogin(t *testing.T) {
	a := &App{
		Store: fakeStore{m: map[string]string{}}, // no credential
		Cfg:   &core.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local", CPBaseURL: "http://x"}}},
	}
	_, err := runCLI(t, a, "health")
	if err == nil || exitCode(err) != 3 {
		t.Fatalf("an unauthenticated gateway command should exit 3, got %v (code %d)", err, exitCode(err))
	}
	if !strings.Contains(err.Error(), "nexus login") {
		t.Fatalf("guard should guide to login: %v", err)
	}
}

func TestGuard_UnknownEnvGuidesToSetup(t *testing.T) {
	a := &App{
		Store: fakeStore{m: map[string]string{}},
		Cfg:   &core.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local", CPBaseURL: "http://x"}}},
	}
	_, err := runCLI(t, a, "--env", "ghost", "health")
	if err == nil || exitCode(err) != 3 {
		t.Fatalf("an unknown env should exit 3, got %v (code %d)", err, exitCode(err))
	}
	if !strings.Contains(err.Error(), "nexus setup") {
		t.Fatalf("guard should guide to setup: %v", err)
	}
}

func TestApp_LoggedInAndOrDefault(t *testing.T) {
	// a stored JWT counts as logged in (not just an admin key).
	a := &App{Env: core.Env{Name: "local"}, Store: fakeStore{m: map[string]string{"local:" + core.SecretAccessToken: "jwt"}}}
	if !a.loggedIn() {
		t.Fatal("a stored access token should count as logged in")
	}
	if (&App{Env: core.Env{Name: "local"}, Store: fakeStore{m: map[string]string{}}}).loggedIn() {
		t.Fatal("no credential is not logged in")
	}
	if orDefault("x", "y") != "x" || orDefault("", "y") != "y" {
		t.Fatal("orDefault wrong")
	}
}

func TestEnvAdd_RmUsageErrors(t *testing.T) {
	a := newConfigApp(t)
	if _, err := runCLIWithIn(t, a, "", "env", "add"); err == nil || exitCode(err) != 2 {
		t.Fatalf("env add with no name should be usage error, got %v", err)
	}
	if _, err := runCLIWithIn(t, a, "", "env", "rm"); err == nil || exitCode(err) != 2 {
		t.Fatalf("env rm with no name should be usage error, got %v", err)
	}
	// explicit --aigw-url is kept (not defaulted to --cp-url).
	if _, err := runCLIWithIn(t, a, "", "env", "add", "p2", "--cp-url", "https://cp", "--aigw-url", "https://sep"); err != nil {
		t.Fatalf("env add: %v", err)
	}
	cfg, _ := core.Load(a.ConfigPath)
	if cfg.Envs["p2"].AIGatewayBaseURL != "https://sep" {
		t.Fatalf("explicit --aigw-url should be kept, got %q", cfg.Envs["p2"].AIGatewayBaseURL)
	}
}

func TestSetup_NameArgAndEditPreservesProd(t *testing.T) {
	a := newConfigApp(t)
	// create "stage" via positional name; prod = yes.
	if _, err := runCLIWithIn(t, a, "https://cp\n\ncp-ui\n\ny\n", "setup", "stage"); err != nil {
		t.Fatalf("setup create: %v", err)
	}
	cfg, _ := core.Load(a.ConfigPath)
	if e := cfg.Envs["stage"]; e.CPBaseURL != "https://cp" || !e.IsProd {
		t.Fatalf("setup create wrong: %+v", e)
	}
	// edit "stage" with all-empty input → every field (incl. prod=yes) preserved.
	a2 := &App{Store: fakeStore{m: map[string]string{}}, ConfigPath: a.ConfigPath}
	if _, err := runCLIWithIn(t, a2, "\n\n\n\n\n", "setup", "stage"); err != nil {
		t.Fatalf("setup edit: %v", err)
	}
	cfg, _ = core.Load(a.ConfigPath)
	if e := cfg.Envs["stage"]; e.CPBaseURL != "https://cp" || e.OAuthClientID != "cp-ui" || !e.IsProd {
		t.Fatalf("setup edit should preserve fields incl. prod: %+v", e)
	}
}

func TestEnvAdd_SetsDefaultWhenNone(t *testing.T) {
	a := newConfigApp(t)
	// remove the built-in local default so default_env becomes empty.
	if _, err := runCLIWithIn(t, a, "", "env", "rm", "local"); err != nil {
		t.Fatalf("env rm local: %v", err)
	}
	a2 := &App{Store: fakeStore{m: map[string]string{}}, ConfigPath: a.ConfigPath}
	if _, err := runCLIWithIn(t, a2, "", "env", "add", "first", "--cp-url", "https://f"); err != nil {
		t.Fatalf("env add: %v", err)
	}
	cfg, _ := core.Load(a.ConfigPath)
	if cfg.DefaultEnv != "first" {
		t.Fatalf("first env added to a default-less config should become default, got %q", cfg.DefaultEnv)
	}
}

func TestEnvCmds_ConfigLoadError(t *testing.T) {
	dir := t.TempDir() // a directory, not a file → core.Load fails to read it
	for _, args := range [][]string{
		{"env", "add", "x", "--cp-url", "https://x"},
		{"env", "rm", "x"},
		{"setup", "x"},
	} {
		a := &App{Store: fakeStore{m: map[string]string{}}, ConfigPath: dir}
		if _, err := runCLIWithIn(t, a, "https://x\n\n\n\n\n", args...); err == nil {
			t.Fatalf("%v with an unreadable config should error", args)
		}
	}
}

func TestTuiDeps_SwitchAndCreateEnv(t *testing.T) {
	a := &App{Store: fakeStore{m: map[string]string{}}, ConfigPath: filepath.Join(t.TempDir(), "c.toml")}
	if err := a.ensureEnv(); err != nil {
		t.Fatalf("ensureEnv: %v", err)
	}
	a.Cfg.SetEnv(core.Env{Name: "dev", CPBaseURL: "https://dev"})
	deps := a.tuiDeps()
	if len(deps.EnvNames) != 2 || deps.EnvNames[0] != "dev" || deps.EnvNames[1] != "local" {
		t.Fatalf("EnvNames should be sorted [dev local], got %v", deps.EnvNames)
	}

	// SwitchEnv rebuilds the client, mutates the active env, and reports no creds.
	gw, sess, loggedIn, err := deps.SwitchEnv("dev")
	if err != nil || gw == nil || sess.EnvName != "dev" || a.Env.Name != "dev" || loggedIn {
		t.Fatalf("SwitchEnv(dev) wrong: gw=%v sess=%+v env=%q loggedIn=%v err=%v", gw != nil, sess, a.Env.Name, loggedIn, err)
	}
	// switching to an unknown env errors.
	if _, _, _, err := deps.SwitchEnv("ghost"); err == nil {
		t.Fatal("SwitchEnv(ghost) should error")
	}

	// CreateEnv persists a new prod env, sets it default, and switches to it.
	gw, sess, err = deps.CreateEnv("prod", "https://prod", true)
	if err != nil || gw == nil || sess.EnvName != "prod" || a.Env.Name != "prod" {
		t.Fatalf("CreateEnv wrong: sess=%+v env=%q err=%v", sess, a.Env.Name, err)
	}
	cfg, _ := core.Load(a.ConfigPath)
	if e := cfg.Envs["prod"]; e.CPBaseURL != "https://prod" || e.AIGatewayBaseURL != "https://prod" || !e.IsProd {
		t.Fatalf("CreateEnv did not persist correctly: %+v", e)
	}
	if cfg.DefaultEnv != "prod" {
		t.Fatalf("CreateEnv should set the new env as default, got %q", cfg.DefaultEnv)
	}

	// HasSession reflects a stored credential for the active env.
	if deps.HasSession() {
		t.Fatal("no credential → HasSession false")
	}
	_ = a.Store.Set("prod", core.SecretAdminKey, "nxk_x")
	if !deps.HasSession() {
		t.Fatal("stored admin key → HasSession true")
	}

	// the persistence closures store against the (now switched) active env.
	if err := deps.SaveVKSecret("nvk_s"); err != nil {
		t.Fatalf("SaveVKSecret: %v", err)
	}
	if v, _ := a.Store.Get("prod", core.SecretVKSecret); v != "nvk_s" {
		t.Fatalf("SaveVKSecret should store for the active env, got %q", v)
	}
	if err := deps.SaveSelection("m1", "vk1", "eng"); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	if a.Env.LastModel != "m1" || a.Cfg.Envs["prod"].LastVKID != "vk1" {
		t.Fatalf("SaveSelection should persist the remembered selection: %+v", a.Cfg.Envs["prod"])
	}
	// CreateVK runs through the client; with no server it surfaces a transport
	// error (the closure body is what we are covering here).
	if _, _, _, err := deps.CreateVK(context.Background(), "x"); err == nil {
		t.Fatal("CreateVK with no reachable gateway should error")
	}
}

func TestEnvUse_Errors(t *testing.T) {
	a := newConfigApp(t)
	// wrong arg count.
	if _, err := runCLIWithIn(t, a, "", "env", "use"); err == nil || exitCode(err) != 2 {
		t.Fatalf("env use with no name should be usage error, got %v", err)
	}
	// unknown env.
	if _, err := runCLIWithIn(t, a, "", "env", "use", "ghost"); err == nil {
		t.Fatal("env use of an unknown env should error")
	}
	// ls renders the built-in local default with its marker.
	out, err := runCLIWithIn(t, a, "", "env", "ls")
	if err != nil || !strings.Contains(out, "local") || !strings.Contains(out, "*") {
		t.Fatalf("env ls should mark the default: %q err=%v", out, err)
	}
}

func TestGuard_ConfigCommandsSkipAuth(t *testing.T) {
	// `env ls` manages config and must work with no credential (skipLoad).
	a := &App{Store: fakeStore{m: map[string]string{}}, Cfg: &core.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": {Name: "local", CPBaseURL: "http://x"}}}}
	if _, err := runCLI(t, a, "env", "ls"); err != nil {
		t.Fatalf("env ls should not require a credential: %v", err)
	}
}
