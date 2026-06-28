package core

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGeneratePKCE_ChallengeMatchesVerifier(t *testing.T) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatal("empty verifier/challenge")
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge %q != S256(verifier) %q", challenge, want)
	}
}

func TestAuthorizeURL_Params(t *testing.T) {
	a := NewAuthenticator(Env{CPBaseURL: "http://cp", OAuthClientID: "cp-ui"}, newMemStore(), nil)
	u, err := url.Parse(a.authorizeURL("CHAL", "STATE", "http://127.0.0.1:9/callback"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             "cp-ui",
		"redirect_uri":          "http://127.0.0.1:9/callback",
		"code_challenge":        "CHAL",
		"code_challenge_method": "S256",
		"state":                 "STATE",
		"scope":                 "openid",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("authorize param %s = %q, want %q", k, got, want)
		}
	}
	if !strings.HasPrefix(u.String(), "http://cp/oauth/authorize?") {
		t.Errorf("authorize URL base wrong: %s", u.String())
	}
}

// headlessServer stands in for the auth server's three endpoints.
func headlessServer(t *testing.T, accessTok, refreshTok string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/authorize":
			w.Header().Set("Location", "/login?authctx=CTX-123")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/authserver/password":
			var body struct{ Authctx, Email, Password string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Authctx != "CTX-123" || body.Email == "" || body.Password == "" {
				http.Error(w, "bad creds", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"redirectUri": "http://localhost:3000/auth/callback?code=CODE-456&state=s",
			})
		case r.URL.Path == "/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "CODE-456" {
				http.Error(w, "bad grant", http.StatusBadRequest)
				return
			}
			if r.Form.Get("code_verifier") == "" {
				http.Error(w, "missing verifier", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: accessTok, RefreshToken: refreshTok, ExpiresIn: 3600})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestLoginHeadless_Success(t *testing.T) {
	at := makeTestJWT(t, time.Now().Add(time.Hour))
	srv := headlessServer(t, at, "refresh-xyz")
	defer srv.Close()

	store := newMemStore()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthClientID: "cp-ui", OAuthRedirectURI: "http://localhost:3000/auth/callback"}, store, srv.Client())

	if err := a.LoginHeadless(context.Background(), "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("LoginHeadless: %v", err)
	}
	if got, _ := store.Get("local", SecretAccessToken); got != at {
		t.Fatalf("access token not stored: %q", got)
	}
	if got, _ := store.Get("local", SecretRefreshToken); got != "refresh-xyz" {
		t.Fatalf("refresh token not stored: %q", got)
	}
}

func TestLoginHeadless_BadPassword(t *testing.T) {
	srv := headlessServer(t, "tok", "ref")
	defer srv.Close()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthRedirectURI: "http://localhost:3000/auth/callback"}, newMemStore(), srv.Client())
	if err := a.LoginHeadless(context.Background(), "", ""); err == nil {
		t.Fatal("want error for empty credentials")
	}
}

func TestLoginHeadless_AuthorizeNoLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // no Location header
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthRedirectURI: "http://x/cb"}, newMemStore(), srv.Client())
	if err := a.LoginHeadless(context.Background(), "e", "p"); err == nil {
		t.Fatal("want error when authorize does not redirect")
	}
}

func TestLoginBrowser_Success(t *testing.T) {
	at := makeTestJWT(t, time.Now().Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_ = r.ParseForm()
			if r.Form.Get("code") != "browser-code" || r.Form.Get("code_verifier") == "" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: at, RefreshToken: "br-ref", ExpiresIn: 3600})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	store := newMemStore()
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: srv.URL, OAuthClientID: "cp-ui"}, store, srv.Client())
	// Simulate the browser: open => GET the loopback callback with code+state.
	a.openBrowser = func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirect := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")
		resp, err := http.Get(redirect + "?code=browser-code&state=" + url.QueryEscape(state))
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.LoginBrowser(ctx); err != nil {
		t.Fatalf("LoginBrowser: %v", err)
	}
	if got, _ := store.Get("local", SecretAccessToken); got != at {
		t.Fatalf("access token not stored: %q", got)
	}
}

func TestLoginBrowser_StateMismatch(t *testing.T) {
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: "http://unused", OAuthClientID: "cp-ui"}, newMemStore(), http.DefaultClient)
	a.openBrowser = func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirect := u.Query().Get("redirect_uri")
		resp, err := http.Get(redirect + "?code=x&state=WRONG-STATE")
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.LoginBrowser(ctx); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("want state mismatch error, got %v", err)
	}
}

func TestLoginBrowser_OpenBrowserFails(t *testing.T) {
	a := NewAuthenticator(Env{Name: "local", CPBaseURL: "http://unused"}, newMemStore(), http.DefaultClient)
	a.openBrowser = func(string) error { return context.Canceled }
	if err := a.LoginBrowser(context.Background()); err == nil {
		t.Fatal("want error when opening browser fails")
	}
}

func TestCallbackHandler_Paths(t *testing.T) {
	t.Run("error param", func(t *testing.T) {
		codeCh, errCh := make(chan string, 1), make(chan error, 1)
		h := callbackHandler("S", codeCh, errCh)
		req := httptest.NewRequest(http.MethodGet, "/callback?error=access_denied&state=S", nil)
		h(httptest.NewRecorder(), req)
		if err := <-errCh; err == nil {
			t.Fatal("want error on error param")
		}
	})
	t.Run("state mismatch", func(t *testing.T) {
		codeCh, errCh := make(chan string, 1), make(chan error, 1)
		h := callbackHandler("S", codeCh, errCh)
		req := httptest.NewRequest(http.MethodGet, "/callback?code=c&state=OTHER", nil)
		h(httptest.NewRecorder(), req)
		if err := <-errCh; err == nil {
			t.Fatal("want state mismatch error")
		}
	})
	t.Run("missing code", func(t *testing.T) {
		codeCh, errCh := make(chan string, 1), make(chan error, 1)
		h := callbackHandler("S", codeCh, errCh)
		req := httptest.NewRequest(http.MethodGet, "/callback?state=S", nil)
		h(httptest.NewRecorder(), req)
		if err := <-errCh; err == nil {
			t.Fatal("want missing-code error")
		}
	})
	t.Run("success", func(t *testing.T) {
		codeCh, errCh := make(chan string, 1), make(chan error, 1)
		h := callbackHandler("S", codeCh, errCh)
		req := httptest.NewRequest(http.MethodGet, "/callback?code=GOOD&state=S", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if code := <-codeCh; code != "GOOD" {
			t.Fatalf("code = %q", code)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestExchangeCode_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad code", http.StatusBadRequest)
	}))
	defer srv.Close()
	a := NewAuthenticator(Env{CPBaseURL: srv.URL, OAuthClientID: "cp-ui"}, newMemStore(), srv.Client())
	if _, err := a.exchangeCode(context.Background(), "c", "v", "http://x/cb"); err == nil {
		t.Fatal("want error on token endpoint 400")
	}
}

func TestBrowserCommand(t *testing.T) {
	// Pure mapping test — never executes anything, never opens a browser.
	cases := []struct {
		goos string
		cmd  string
		args []string
	}{
		{"darwin", "open", []string{"http://x"}},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", "http://x"}},
		{"linux", "xdg-open", []string{"http://x"}},
	}
	for _, tc := range cases {
		cmd, args := browserCommand(tc.goos, "http://x")
		if cmd != tc.cmd || len(args) != len(tc.args) {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", tc.goos, cmd, args, tc.cmd, tc.args)
			continue
		}
		for i := range args {
			if args[i] != tc.args[i] {
				t.Errorf("%s arg[%d] = %q, want %q", tc.goos, i, args[i], tc.args[i])
			}
		}
	}
}
