package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// TestSave_AConcurrentReaderNeverSeesAPartialConfig pins the property the
// truncate destroyed.
//
// Save used to open the real path with O_CREATE|O_TRUNC|O_WRONLY and encode
// into it. That empties the file BEFORE the replacement exists, so for the
// duration of the encode the config on disk is short or empty — and anything
// that goes wrong in that window (a full disk, a failing close, the process
// dying) leaves the operator with no environments rather than the ones they
// had. A reader that arrives mid-write sees the same emptiness.
//
// The assertion is not "Save works" — the round-trip test covers that. It is
// that the file is ALWAYS complete when observed, which is what temp-then-
// rename buys and what O_TRUNC cannot provide at any speed.
func TestSave_AConcurrentReaderNeverSeesAPartialConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "config.toml")
	c := sampleConfig(path)
	// Enough environments that the encode is not a single small write.
	for i := range 40 {
		name := fmt.Sprintf("env-%02d", i)
		c.Envs[name] = core.Env{
			Name:      name,
			CPBaseURL: "https://cp-" + name + ".example.com/a/reasonably/long/path",
			LastModel: "some-model-identifier-" + name,
		}
	}
	if err := c.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}
	wantLen := len(full)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var short atomic.Int64
	var reads atomic.Int64

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			reads.Add(1)
			b, err := os.ReadFile(path)
			if err != nil {
				// ENOENT means the config briefly did not exist at all — the
				// worst form of the same defect, so it counts as an observation
				// AND as a failure. Counting it only as a failure would let an
				// always-missing file trip the "asserts nothing" guard below and
				// misreport a real break as an inconclusive test.
				short.Add(1)
				continue
			}
			if len(b) != wantLen {
				short.Add(1)
			}
		}
	}()

	for range 50 {
		if err := c.Save(); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("save: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	if reads.Load() == 0 {
		t.Fatal("the reader never observed the file, so this asserts nothing")
	}
	if n := short.Load(); n != 0 {
		t.Errorf("%d of %d reads saw an incomplete or missing config; Save must replace it "+
			"atomically, not truncate it and refill", n, reads.Load())
	}
}

// TestSave_TempCreationFails covers the path where the replacement file cannot
// be created at all. The guarantee under test is that the EXISTING config
// survives: the whole point of writing a sibling and renaming is that a failure
// before the rename leaves the operator's environments exactly where they were.
func TestSave_TempCreationFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	c := sampleConfig(path)
	if err := c.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}

	// Read+execute only: MkdirAll on an existing dir succeeds, and CreateTemp
	// then fails for want of write permission. Root bypasses that check
	// entirely, so the premise of this test does not hold there — chmod
	// succeeds, the save succeeds, and the failure would be reported as a
	// missing error rather than as an environment that cannot express the case.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so an unwritable directory cannot be simulated")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot drop write permission on %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c.DefaultEnv = "changed"
	err = c.Save()
	if err == nil {
		t.Fatal("Save into an unwritable directory must fail")
	}
	if !strings.Contains(err.Error(), "temp config") {
		t.Errorf("error should name the step that failed, got %v", err)
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the previous config must still be readable after a failed save: %v", readErr)
	}
	if string(after) != string(before) {
		t.Errorf("a failed save changed the config on disk;\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestSave_FollowsASymlinkedConfig pins what temp-then-rename would otherwise
// break. O_TRUNC wrote THROUGH a symlink, so a config managed by a dotfile tool
// (stow, chezmoi, home-manager) kept its link and its real file was updated.
// A bare rename replaces the LINK with a regular file: the link is gone, the
// real file is frozen at its old contents, and Save reports success. Save
// resolves the path first so the file the operator actually manages is the one
// that changes.
func TestSave_FollowsASymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.toml")
	link := filepath.Join(dir, "config.toml")

	seed := sampleConfig(real)
	if err := seed.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	c := sampleConfig(link)
	c.DefaultEnv = "through-the-link"
	if err := c.Save(); err != nil {
		t.Fatalf("save through symlink: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("Save replaced the symlink with a regular file; a dotfile-managed config " +
			"would lose its link and every later edit to the real file")
	}
	reloaded, err := Load(real)
	if err != nil {
		t.Fatalf("reload the real file: %v", err)
	}
	if reloaded.DefaultEnv != "through-the-link" {
		t.Errorf("the real file was not updated: DefaultEnv = %q", reloaded.DefaultEnv)
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
