package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
)

// vkTestApp builds an App whose config has a writable path (so SaveSelection
// works) and whose client talks to srv via a stored admin key.
func vkTestApp(t *testing.T, srv *httptest.Server) *App {
	t.Helper()
	cfg, err := local.Load(t.TempDir() + "/config.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	env := cfg.Envs["local"]
	env.CPBaseURL = srv.URL
	cfg.SetEnv(env)
	return &App{
		Cfg: cfg, Env: env,
		Store: fakeStore{m: map[string]string{"local:" + core.SecretAdminKey: "nxk_test"}},
		HTTP:  srv.Client(),
	}
}

func TestVKCreate_PrintsAndStores(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/virtual-keys" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"vk-new","name":"nexus-cli","keyPrefix":"nvk_abc","key":"nvk_plaintext_once"}`)
	}))
	defer srv.Close()

	// store=true (default): prints the secret once + stores it in the keychain
	// and persists the selection (needs a config with a writable path).
	a := vkTestApp(t, srv)
	out, err := runCLI(t, a, "vk", "create", "--name", "nexus-cli")
	if err != nil || !strings.Contains(out, "nvk_plaintext_once") || !strings.Contains(out, "stored as this env") {
		t.Fatalf("vk create wrong: %q err=%v", out, err)
	}
	if got, _ := a.Store.Get("local", core.SecretVKSecret); got != "nvk_plaintext_once" {
		t.Fatalf("secret not stored: %q", got)
	}

	// --output json emits the full CreatedVK.
	out, err = runCLI(t, vkTestApp(t, srv), "vk", "create", "-o", "json")
	if err != nil || !strings.Contains(out, `"key": "nvk_plaintext_once"`) {
		t.Fatalf("vk create json wrong: %q err=%v", out, err)
	}

	// --store=false: prints but does not persist.
	a2 := newTestApp(srv, false)
	if _, err := runCLI(t, a2, "vk", "create", "--store=false"); err != nil {
		t.Fatalf("vk create --store=false: %v", err)
	}
	if _, err := a2.Store.Get("local", core.SecretVKSecret); err == nil {
		t.Fatal("--store=false should not persist the secret")
	}
}

func TestVKCreate_StoreError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"vk-new","name":"x","key":"nvk_x"}`)
	}))
	defer srv.Close()
	a := vkTestApp(t, srv)
	// keychain write fails → vk create surfaces the error (store=true default)
	a.Store = fakeStore{m: map[string]string{"local:" + core.SecretAdminKey: "nxk"}, setErr: errors.New("keychain locked")}
	if _, err := runCLI(t, a, "vk", "create"); err == nil || !strings.Contains(err.Error(), "keychain locked") {
		t.Fatalf("store error should surface, got %v", err)
	}
}

func TestVKCreate_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"denied"}}`)
	}))
	defer srv.Close()
	if _, err := runCLI(t, newTestApp(srv, false), "vk", "create"); exitCode(err) != 4 {
		t.Fatalf("403 should map to exit 4, got %d", exitCode(err))
	}
}
