package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func sampleConfig(path string) *Config {
	return &Config{
		DefaultEnv: "local",
		Envs: map[string]core.Env{
			"local": {Name: "local", CPBaseURL: "http://localhost:3001", OAuthClientID: "cp-ui"},
			"prod":  {Name: "prod", CPBaseURL: "https://cp.example.com", OAuthClientID: "cp-ui", IsProd: true},
		},
		path: path,
	}
}

func TestResolve_Precedence(t *testing.T) {
	c := sampleConfig("")
	// flag wins over session and default.
	got, err := c.Resolve("prod", "local")
	if err != nil || got.Name != "prod" {
		t.Fatalf("flag precedence: got %q err=%v, want prod", got.Name, err)
	}
	// session wins over default when no flag.
	got, err = c.Resolve("", "prod")
	if err != nil || got.Name != "prod" {
		t.Fatalf("session precedence: got %q err=%v, want prod", got.Name, err)
	}
	// default when neither flag nor session.
	got, err = c.Resolve("", "")
	if err != nil || got.Name != "local" {
		t.Fatalf("default precedence: got %q err=%v, want local", got.Name, err)
	}
}

func TestResolve_UnknownEnv(t *testing.T) {
	c := sampleConfig("")
	_, err := c.Resolve("ghost", "")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want unknown-env error naming ghost, got %v", err)
	}
}

func TestResolve_NoSelection(t *testing.T) {
	c := &Config{Envs: map[string]core.Env{}} // no default, no envs
	_, err := c.Resolve("", "")
	if err == nil {
		t.Fatal("want error when nothing resolves, got nil")
	}
}

func TestResolve_FillsNameFromKey(t *testing.T) {
	c := &Config{DefaultEnv: "x", Envs: map[string]core.Env{"x": {CPBaseURL: "u"}}} // Env.Name empty
	got, err := c.Resolve("", "")
	if err != nil || got.Name != "x" {
		t.Fatalf("want Name filled from map key, got %q err=%v", got.Name, err)
	}
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if cfg.DefaultEnv != "local" {
		t.Fatalf("default config DefaultEnv = %q, want local", cfg.DefaultEnv)
	}
	env, err := cfg.Resolve("", "")
	if err != nil || env.CPBaseURL != "http://localhost:3001" {
		t.Fatalf("default local env wrong: %+v err=%v", env, err)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "config.toml")
	c := sampleConfig(path)
	c.Envs["prod"] = core.Env{Name: "prod", CPBaseURL: "https://cp.example.com", LastModel: "gpt-4", LastVKName: "research"}
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File must be 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config perm = %o, want 600", perm)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DefaultEnv != "local" {
		t.Fatalf("DefaultEnv lost: %q", reloaded.DefaultEnv)
	}
	if reloaded.Envs["prod"].LastModel != "gpt-4" || reloaded.Envs["prod"].LastVKName != "research" {
		t.Fatalf("env fields lost on round-trip: %+v", reloaded.Envs["prod"])
	}
}

func TestSave_NoPath(t *testing.T) {
	if err := (&Config{}).Save(); err == nil {
		t.Fatal("save with empty path should error")
	}
}

func TestSetEnvAndSetDefault(t *testing.T) {
	c := &Config{Envs: nil}
	c.SetEnv(core.Env{Name: "dev", CPBaseURL: "u"})
	if c.Envs["dev"].CPBaseURL != "u" {
		t.Fatal("SetEnv did not insert")
	}
	if err := c.SetDefault("dev"); err != nil {
		t.Fatalf("SetDefault dev: %v", err)
	}
	if c.DefaultEnv != "dev" {
		t.Fatal("SetDefault did not set")
	}
	if err := c.SetDefault("ghost"); err == nil {
		t.Fatal("SetDefault ghost should error")
	}
}

func TestRemoveEnv(t *testing.T) {
	c := &Config{DefaultEnv: "dev", Envs: map[string]core.Env{"dev": {Name: "dev"}, "local": builtinLocalEnv()}}
	// removing the current default clears default_env.
	if err := c.RemoveEnv("dev"); err != nil {
		t.Fatalf("RemoveEnv dev: %v", err)
	}
	if _, ok := c.Envs["dev"]; ok {
		t.Fatal("dev should be removed")
	}
	if c.DefaultEnv != "" {
		t.Fatalf("removing the default should clear default_env, got %q", c.DefaultEnv)
	}
	// removing a non-default env keeps the default.
	c.DefaultEnv = "local"
	c.Envs["extra"] = core.Env{Name: "extra"}
	if err := c.RemoveEnv("extra"); err != nil {
		t.Fatalf("RemoveEnv extra: %v", err)
	}
	if c.DefaultEnv != "local" {
		t.Fatal("removing a non-default env must keep the default")
	}
	// removing an unknown env errors.
	if err := c.RemoveEnv("ghost"); err == nil {
		t.Fatal("RemoveEnv ghost should error")
	}
}

func TestConfig_PersistsNoSecrets(t *testing.T) {
	// The Env type has no secret field, so a saved config can never contain a
	// token even after we store secrets out-of-band. Prove it: store secrets in
	// the keychain, save config, assert the file bytes contain none of them.
	keyring.MockInit()
	path := filepath.Join(t.TempDir(), "config.toml")
	var store KeyringStore
	_ = store.Set("local", core.SecretAccessToken, "SECRET-ACCESS-TOKEN-XYZ")
	_ = store.Set("local", core.SecretAdminKey, "nxk_SECRET_ADMIN_KEY")
	c := sampleConfig(path)
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, secret := range []string{"SECRET-ACCESS-TOKEN-XYZ", "nxk_SECRET_ADMIN_KEY"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("config file leaked secret %q", secret)
		}
	}
}

func TestDefaultConfigPath(t *testing.T) {
	p, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if !strings.HasSuffix(p, filepath.Join("nexus", "config.toml")) {
		t.Fatalf("unexpected path %q", p)
	}
}

func TestLoad_ParseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("this is = = not toml ]["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("want parse error for malformed toml")
	}
}

func TestConfig_SaveMkdirFails(t *testing.T) {
	// A regular file in the path's parent chain makes MkdirAll fail.
	fileAsDir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := sampleConfig(filepath.Join(fileAsDir, "sub", "config.toml"))
	if err := c.Save(); err == nil {
		t.Fatal("Save should fail when a parent path element is a file")
	}
}

func TestConfig_LoadReadError(t *testing.T) {
	// Reading a directory as a file returns a non-IsNotExist error.
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("Load of a directory path should error")
	}
}

func TestConfig_LoadEnvsNilInitialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte(`default_env = "local"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Envs == nil {
		t.Fatal("Envs should be initialized to a non-nil map")
	}
}
