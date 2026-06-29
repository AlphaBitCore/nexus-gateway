package core

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewTokenSource_SelectsByStoredCredential(t *testing.T) {
	env := Env{Name: "local", CPBaseURL: "http://x"}

	withKey := newMemStore()
	_ = withKey.Set("local", SecretAdminKey, "nxk_abc")
	if _, ok := NewTokenSource(env, withKey, nil).(*apiKeyTokenSource); !ok {
		t.Fatal("with admin key stored, want apiKeyTokenSource")
	}

	noKey := newMemStore()
	if _, ok := NewTokenSource(env, noKey, nil).(*jwtTokenSource); !ok {
		t.Fatal("without admin key, want jwtTokenSource")
	}
}

func TestAPIKeyTokenSource_Credential(t *testing.T) {
	store := newMemStore()
	_ = store.Set("local", SecretAdminKey, "nxk_secret")
	src := &apiKeyTokenSource{env: Env{Name: "local"}, store: store}

	h, v, err := src.Credential(context.Background())
	if err != nil || h != "x-admin-key" || v != "nxk_secret" {
		t.Fatalf("credential = (%q,%q) err=%v", h, v, err)
	}

	src2 := &apiKeyTokenSource{env: Env{Name: "empty"}, store: newMemStore()}
	if _, _, err := src2.Credential(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing key: want ErrUnauthorized, got %v", err)
	}
}

func newJWTSource(t *testing.T, store SecretStore, cpURL string, now time.Time) *jwtTokenSource {
	t.Helper()
	return &jwtTokenSource{
		env:   Env{Name: "local", CPBaseURL: cpURL, OAuthClientID: "cp-ui"},
		store: store,
		httpc: http.DefaultClient,
		now:   func() time.Time { return now },
		skew:  defaultRefreshSkew,
	}
}

func TestJWTTokenSource_ValidToken(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	tok := makeTestJWT(t, now.Add(time.Hour))
	_ = store.Set("local", SecretAccessToken, tok)
	src := newJWTSource(t, store, "http://unused", now)

	h, v, err := src.Credential(context.Background())
	if err != nil || h != "Authorization" || v != "Bearer "+tok {
		t.Fatalf("valid token credential = (%q,%q) err=%v", h, v, err)
	}
}

func TestJWTTokenSource_RefreshesNearExpiry(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	// Access token within the refresh skew window.
	_ = store.Set("local", SecretAccessToken, makeTestJWT(t, now.Add(10*time.Second)))
	_ = store.Set("local", SecretRefreshToken, "refresh-1")

	newTok := makeTestJWT(t, now.Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" {
			t.Errorf("bad refresh form: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: newTok, RefreshToken: "refresh-2", ExpiresIn: 3600})
	}))
	defer srv.Close()

	src := newJWTSource(t, store, srv.URL, now)
	_, v, err := src.Credential(context.Background())
	if err != nil || v != "Bearer "+newTok {
		t.Fatalf("after refresh credential v=%q err=%v", v, err)
	}
	// New tokens persisted.
	if got, _ := store.Get("local", SecretAccessToken); got != newTok {
		t.Fatalf("access token not updated: %q", got)
	}
	if got, _ := store.Get("local", SecretRefreshToken); got != "refresh-2" {
		t.Fatalf("refresh token not rotated: %q", got)
	}
}

func TestJWTTokenSource_RefreshFailsButTokenStillValid(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	stillValid := makeTestJWT(t, now.Add(10*time.Second)) // inside skew, not expired
	_ = store.Set("local", SecretAccessToken, stillValid)
	_ = store.Set("local", SecretRefreshToken, "refresh-1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	src := newJWTSource(t, store, srv.URL, now)
	_, v, err := src.Credential(context.Background())
	if err != nil || v != "Bearer "+stillValid {
		t.Fatalf("refresh-fail-but-valid: v=%q err=%v (want old token)", v, err)
	}
}

func TestJWTTokenSource_ExpiredAndRefreshFails(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	_ = store.Set("local", SecretAccessToken, makeTestJWT(t, now.Add(-time.Hour))) // expired
	// no refresh token stored → refresh fails
	src := newJWTSource(t, store, "http://unused", now)

	if _, _, err := src.Credential(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired+no-refresh: want ErrUnauthorized, got %v", err)
	}
}

func TestJWTTokenSource_NoAccessToken(t *testing.T) {
	src := newJWTSource(t, newMemStore(), "http://unused", time.Now())
	if _, _, err := src.Credential(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no token: want ErrUnauthorized, got %v", err)
	}
}

func TestJWTTokenSource_RefreshResponseNoToken(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	_ = store.Set("local", SecretAccessToken, makeTestJWT(t, now.Add(-time.Hour)))
	_ = store.Set("local", SecretRefreshToken, "refresh-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{}) // empty access_token
	}))
	defer srv.Close()
	src := newJWTSource(t, store, srv.URL, now)
	if _, _, err := src.Credential(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty refresh response: want ErrUnauthorized, got %v", err)
	}
}
