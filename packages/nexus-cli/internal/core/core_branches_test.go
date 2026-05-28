package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseAPIError_BodyFallbacks(t *testing.T) {
	// Empty body → human status text.
	e := parseAPIError(http.StatusNotFound, nil)
	if e.Message != http.StatusText(http.StatusNotFound) {
		t.Fatalf("empty body message = %q, want %q", e.Message, http.StatusText(404))
	}
	// Envelope present but message empty → falls back to raw body text.
	e2 := parseAPIError(http.StatusBadGateway, []byte(`{"error":{"code":"X"}}`))
	if !strings.Contains(e2.Message, `"error"`) {
		t.Fatalf("no-message envelope should fall back to body, got %q", e2.Message)
	}
}

func TestDo_MarshalError(t *testing.T) {
	c := NewClient(Env{Name: "local", CPBaseURL: "http://unused"},
		fixedTokenSource{header: "Authorization", value: "Bearer T"}, http.DefaultClient)
	// A channel cannot be JSON-marshaled → marshal error before any network I/O.
	err := c.do(context.Background(), http.MethodPost, c.env.CPBaseURL, "/x", nil, make(chan int), nil)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("want ErrTransport on unmarshalable body, got %v", err)
	}
}

func TestLoginBrowser_Timeout(t *testing.T) {
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: "http://unused", OAuthClientID: "cp-ui"}, newMemStore(), http.DefaultClient)
	a.openBrowser = func(string) error { return nil } // never completes the callback
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.LoginBrowser(ctx); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestLoginBrowser_ExchangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad code", http.StatusBadRequest) // /oauth/token fails
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthClientID: "cp-ui"}, newMemStore(), srv.Client())
	a.openBrowser = func(authURL string) error {
		u, _ := parseQueryURL(authURL)
		resp, err := http.Get(u.redirect + "?code=c&state=" + u.state)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.LoginBrowser(ctx); err == nil {
		t.Fatal("want error when token exchange fails after callback")
	}
}

func TestFetchAuthctx_NoAuthctx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/login?foo=bar") // 302 but no authctx
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{CPBaseURL: srv.URL, OAuthClientID: "cp-ui"}, newMemStore(), srv.Client())
	if _, err := a.fetchAuthctx(context.Background(), "c", "s", "http://x/cb"); err == nil {
		t.Fatal("want error when Location lacks authctx")
	}
}

func TestPasswordExchange_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			w.Header().Set("Location", "/login?authctx=CTX-123")
			w.WriteHeader(http.StatusFound)
		case "/authserver/password":
			_, _ = w.Write([]byte("not json"))
		}
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthRedirectURI: "http://x/cb"}, newMemStore(), srv.Client())
	if err := a.LoginHeadless(context.Background(), "e", "p"); err == nil {
		t.Fatal("want decode error from /authserver/password")
	}
}

func TestExchangeCode_RequestError(t *testing.T) {
	a := NewAuthenticator(Env{CPBaseURL: "http://127.0.0.1:0", OAuthClientID: "cp-ui"}, newMemStore(), &http.Client{Timeout: time.Second})
	if _, err := a.exchangeCode(context.Background(), "c", "v", "http://x/cb"); err == nil {
		t.Fatal("want error when token request cannot connect")
	}
}

func TestStoreTokens_RefreshSetFails(t *testing.T) {
	a := NewAuthenticator(Env{Name: "local"}, refreshFailStore{newMemStore()}, http.DefaultClient)
	if err := a.storeTokens(tokenResponse{AccessToken: "a", RefreshToken: "r"}); err == nil {
		t.Fatal("storeTokens should fail when refresh-token Set fails")
	}
}

func TestLoginHeadless_TokenExchangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			w.Header().Set("Location", "/login?authctx=CTX-123")
			w.WriteHeader(http.StatusFound)
		case "/authserver/password":
			_, _ = w.Write([]byte(`{"redirectUri":"http://x/cb?code=C&state=s"}`))
		case "/oauth/token":
			http.Error(w, "bad grant", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthRedirectURI: "http://x/cb"}, newMemStore(), srv.Client())
	if err := a.LoginHeadless(context.Background(), "e", "p"); err == nil {
		t.Fatal("want error when token exchange fails in headless flow")
	}
}

func TestExchangeCode_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json")) // 200 but undecodable
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{CPBaseURL: srv.URL, OAuthClientID: "cp-ui"}, newMemStore(), srv.Client())
	if _, err := a.exchangeCode(context.Background(), "c", "v", "http://x/cb"); err == nil {
		t.Fatal("want decode error from /oauth/token")
	}
}

func TestPasswordExchange_RequestError(t *testing.T) {
	a := NewAuthenticator(Env{CPBaseURL: "http://127.0.0.1:0"}, newMemStore(), &http.Client{Timeout: time.Second})
	if _, err := a.passwordExchange(context.Background(), "ctx", "e", "p"); err == nil {
		t.Fatal("want error when password request cannot connect")
	}
}

func TestRefresh_RequestError(t *testing.T) {
	now := time.Now()
	store := newMemStore()
	_ = store.Set("local", SecretAccessToken, makeTestJWT(t, now.Add(-time.Hour))) // expired
	_ = store.Set("local", SecretRefreshToken, "r1")
	src := &jwtTokenSource{
		env:   Env{Name: "local", CPBaseURL: "http://127.0.0.1:0", OAuthClientID: "cp-ui"},
		store: store, httpc: &http.Client{Timeout: time.Second}, now: func() time.Time { return now }, skew: defaultRefreshSkew,
	}
	if _, _, err := src.Credential(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh connect failure + expired: want ErrUnauthorized, got %v", err)
	}
}

func TestWithBrowserOpener(t *testing.T) {
	a := NewAuthenticator(Env{Name: "x"}, newMemStore(), http.DefaultClient)
	called := false
	got := a.WithBrowserOpener(func(string) error { called = true; return nil })
	if got != a {
		t.Fatal("WithBrowserOpener should return the same authenticator")
	}
	_ = a.openBrowser("http://x")
	if !called {
		t.Fatal("custom opener was not installed")
	}
	// nil must keep the previously installed opener.
	called = false
	a.WithBrowserOpener(nil)
	_ = a.openBrowser("http://x")
	if !called {
		t.Fatal("WithBrowserOpener(nil) must not clear the opener")
	}
}

// refreshFailStore fails only when storing the refresh token.
type refreshFailStore struct{ *memSecretStore }

func (s refreshFailStore) Set(env, key, val string) error {
	if key == SecretRefreshToken {
		return errors.New("refresh write failed")
	}
	return s.memSecretStore.Set(env, key, val)
}

// parseQueryURL extracts the redirect_uri and state from an authorize URL.
type authParts struct{ redirect, state string }

func parseQueryURL(authURL string) (authParts, error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return authParts{}, err
	}
	q := u.Query()
	return authParts{redirect: q.Get("redirect_uri"), state: q.Get("state")}, nil
}
