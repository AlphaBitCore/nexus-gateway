package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// TestClient_TypedMethods_ErrorPropagation drives every typed method against a
// server that always 500s, asserting the error is surfaced (covers the
// per-method error-return branches).
func TestClient_TypedMethods_ErrorPropagation(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer done()
	ctx := context.Background()

	type call struct {
		name string
		run  func() error
	}
	calls := []call{
		{"Sparkline", func() error { _, e := c.Sparkline(ctx, nil); return e }},
		{"MetricsAggregates", func() error { _, e := c.MetricsAggregates(ctx, nil); return e }},
		{"TrafficList", func() error { _, e := c.TrafficList(ctx, TrafficFilter{}); return e }},
		{"TrafficEvent", func() error { _, e := c.TrafficEvent(ctx, "id"); return e }},
		{"TrafficEventNormalized", func() error { _, e := c.TrafficEventNormalized(ctx, "id"); return e }},
		{"Instances", func() error { _, e := c.Instances(ctx); return e }},
		{"VirtualKeys", func() error { _, e := c.VirtualKeys(ctx); return e }},
		{"SetKillSwitch", func() error { _, e := c.SetKillSwitch(ctx, true); return e }},
		{"AdminModels", func() error { _, e := c.AdminModels(ctx); return e }},
		{"Cost", func() error { _, e := c.Cost(ctx, nil); return e }},
		{"GetJSON", func() error { return c.GetJSON(ctx, "/api/admin/x", nil, &struct{}{}) }},
	}
	for _, cl := range calls {
		if err := cl.run(); !errors.Is(err, ErrTransport) {
			t.Errorf("%s: want ErrTransport from 500, got %v", cl.name, err)
		}
	}
}

func TestAPIError_ErrorString(t *testing.T) {
	withIAM := &APIError{Status: 403, Code: "FORBIDDEN", Message: "denied", IAMAction: "admin:x.read", kind: ErrForbidden}
	if !strings.Contains(withIAM.Error(), "iam: admin:x.read") {
		t.Errorf("IAM error string missing action: %s", withIAM.Error())
	}
	transport := &APIError{Message: "dial fail", kind: ErrTransport} // Status 0
	if !strings.Contains(transport.Error(), "dial fail") || strings.Contains(transport.Error(), "(0") {
		t.Errorf("transport error string wrong: %s", transport.Error())
	}
	statusErr := &APIError{Status: 404, Code: "NOT_FOUND", Message: "missing", kind: ErrNotFound}
	if !strings.Contains(statusErr.Error(), "404") || !strings.Contains(statusErr.Error(), "NOT_FOUND") {
		t.Errorf("status error string wrong: %s", statusErr.Error())
	}
}

func TestKeyringStore_BackendErrors(t *testing.T) {
	backendErr := errors.New("keychain locked")
	keyring.MockInitWithError(backendErr)

	var s KeyringStore
	if _, err := s.Get("local", SecretAccessToken); err == nil || errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Get backend error: want wrapped non-NotFound error, got %v", err)
	}
	if err := s.Set("local", SecretAccessToken, "v"); err == nil {
		t.Errorf("Set backend error: want error")
	}
	if err := s.Delete("local", SecretAccessToken); err == nil {
		t.Errorf("Delete backend error: want error")
	}
	keyring.MockInit() // restore clean mock for other tests
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

func TestRefresh_BadJSONBody(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	_ = store.Set("local", SecretAccessToken, makeTestJWT(t, now.Add(-time.Hour))) // expired
	_ = store.Set("local", SecretRefreshToken, "r1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	src := newJWTSource(t, store, srv.URL, now)
	if _, _, err := src.Credential(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad refresh JSON + expired: want ErrUnauthorized, got %v", err)
	}
}

func TestRefresh_StoreSetFails(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	_ = store.Set("local", SecretAccessToken, makeTestJWT(t, now.Add(10*time.Second)))
	_ = store.Set("local", SecretRefreshToken, "r1")
	at := makeTestJWT(t, now.Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"` + at + `","expires_in":3600}`))
	}))
	defer srv.Close()
	// Now make subsequent Set calls fail so refresh's persist step errors; the
	// token is still inside skew but not expired, so Credential falls back to it.
	store.setErr = errors.New("keychain write failed")
	src := newJWTSource(t, store, srv.URL, now)
	_, v, err := src.Credential(context.Background())
	if err != nil || !strings.HasPrefix(v, "Bearer ") {
		t.Fatalf("refresh-persist-fail fallback: v=%q err=%v", v, err)
	}
}

func TestPasswordExchange_NoCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			w.Header().Set("Location", "/login?authctx=CTX-123")
			w.WriteHeader(http.StatusFound)
		case "/authserver/password":
			_, _ = w.Write([]byte(`{"redirectUri":"http://localhost:3000/auth/callback?state=s"}`)) // no code
		}
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthRedirectURI: "http://localhost:3000/auth/callback"}, newMemStore(), srv.Client())
	if err := a.LoginHeadless(context.Background(), "e", "p"); err == nil {
		t.Fatal("want error when redirectUri carries no code")
	}
}

func TestStoreTokens(t *testing.T) {
	a := NewAuthenticator(Env{Name: "local"}, newMemStore(), http.DefaultClient)
	// access only, no refresh.
	if err := a.storeTokens(tokenResponse{AccessToken: "a"}); err != nil {
		t.Fatalf("storeTokens access-only: %v", err)
	}
	if got, _ := a.store.Get("local", SecretAccessToken); got != "a" {
		t.Fatalf("access not stored: %q", got)
	}
	if _, err := a.store.Get("local", SecretRefreshToken); !errors.Is(err, ErrSecretNotFound) {
		t.Fatal("no refresh should have been stored")
	}
	// access set fails.
	failing := &memSecretStore{m: map[string]string{}, setErr: errors.New("write fail")}
	a2 := NewAuthenticator(Env{Name: "local"}, failing, http.DefaultClient)
	if err := a2.storeTokens(tokenResponse{AccessToken: "a", RefreshToken: "r"}); err == nil {
		t.Fatal("storeTokens should fail when store.Set fails")
	}
}

func TestExchangeCode_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`)) // 200 but no access_token
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{CPBaseURL: srv.URL, OAuthClientID: "cp-ui"}, newMemStore(), srv.Client())
	if _, err := a.exchangeCode(context.Background(), "c", "v", "http://x/cb"); err == nil {
		t.Fatal("want error when token response has no access_token")
	}
}

func TestFetchAuthctx_RequestError(t *testing.T) {
	a := NewAuthenticator(Env{CPBaseURL: "http://127.0.0.1:0", OAuthClientID: "cp-ui"}, newMemStore(), &http.Client{Timeout: time.Second})
	if _, err := a.fetchAuthctx(context.Background(), "chal", "state", "http://x/cb"); err == nil {
		t.Fatal("want error when authorize request cannot connect")
	}
}
