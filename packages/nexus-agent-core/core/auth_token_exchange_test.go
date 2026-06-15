package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		{"TrafficList", func() error { _, e := c.TrafficList(ctx, TrafficFilter{}); return e }},
		{"TrafficEvent", func() error { _, e := c.TrafficEvent(ctx, "id"); return e }},
		{"TrafficEventNormalized", func() error { _, e := c.TrafficEventNormalized(ctx, "id"); return e }},
		{"Instances", func() error { _, e := c.Instances(ctx); return e }},
		{"VirtualKeys", func() error { _, e := c.VirtualKeys(ctx); return e }},
		{"SetKillSwitch", func() error { _, e := c.SetKillSwitch(ctx, true); return e }},
		{"AdminModels", func() error { _, e := c.AdminModels(ctx); return e }},
		{"Cost", func() error { _, e := c.Cost(ctx, nil); return e }},
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
