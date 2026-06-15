package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Authenticator drives the two login flows over the env's CP base URL and
// persists the resulting tokens via the SecretStore. Both flows use OAuth2
// Authorization Code + PKCE (S256) against the existing auth server; neither
// introduces a new auth backend.
type Authenticator struct {
	env         Env
	store       SecretStore
	httpc       *http.Client
	openBrowser func(string) error // injectable; defaults to the OS opener
}

// NewAuthenticator builds an Authenticator for env.
func NewAuthenticator(env Env, store SecretStore, httpc *http.Client) *Authenticator {
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Authenticator{env: env, store: store, httpc: httpc, openBrowser: openInBrowser}
}

// WithBrowserOpener overrides how the authorize URL is opened. The default
// opens the OS browser; callers (and tests) may substitute another opener.
func (a *Authenticator) WithBrowserOpener(fn func(string) error) *Authenticator {
	if fn != nil {
		a.openBrowser = fn
	}
	return a
}

// generatePKCE returns a high-entropy verifier and its S256 challenge.
func generatePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 33)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// randState returns an unguessable CSRF state value. A crypto/rand failure must
// not be swallowed: an empty/predictable state would defeat the OAuth CSRF check,
// so the error is surfaced (matching generatePKCE) and the login aborts.
func randState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate CSRF state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// authorizeURL builds the /oauth/authorize URL for the given PKCE challenge,
// state, and redirect URI.
func (a *Authenticator) authorizeURL(challenge, state, redirectURI string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.env.OAuthClientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"scope":                 {"openid"},
	}
	return strings.TrimRight(a.env.CPBaseURL, "/") + "/oauth/authorize?" + q.Encode()
}

// LoginBrowser runs the interactive browser-loopback PKCE flow: it starts a
// loopback listener, opens the system browser at /oauth/authorize, captures the
// redirected code, and exchanges it for tokens. ctx bounds the wait.
func (a *Authenticator) LoginBrowser(ctx context.Context) error {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return err
	}
	state, err := randState()
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start loopback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: callbackHandler(state, codeCh, errCh)}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	if err := a.openBrowser(a.authorizeURL(challenge, state, redirectURI)); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}

	select {
	case code := <-codeCh:
		tr, err := a.exchangeCode(ctx, code, verifier, redirectURI)
		if err != nil {
			return err
		}
		return a.storeTokens(tr)
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return fmt.Errorf("login timed out waiting for browser redirect: %w", ctx.Err())
	}
}

// callbackHandler completes the loopback redirect, validating state and
// forwarding the code (or error) on the channels.
func callbackHandler(wantState string, codeCh chan<- string, errCh chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "login failed", http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization error: %s", e)
			return
		}
		if q.Get("state") != wantState {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch (possible CSRF)")
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("callback had no authorization code")
			return
		}
		_, _ = io.WriteString(w, "Login complete. You can close this tab and return to the terminal.")
		codeCh <- code
	}
}

// LoginHeadless runs the non-interactive password exchange (email/password)
// against the auth server, mirroring tests/lib/auth.sh. It is used by tests and
// local verification where opening a browser is not possible. It uses the env's
// registered OAuthRedirectURI (not a loopback URI).
func (a *Authenticator) LoginHeadless(ctx context.Context, email, password string) error {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return err
	}
	state, err := randState()
	if err != nil {
		return err
	}
	redirectURI := a.env.OAuthRedirectURI

	authctx, err := a.fetchAuthctx(ctx, challenge, state, redirectURI)
	if err != nil {
		return err
	}
	code, err := a.passwordExchange(ctx, authctx, email, password)
	if err != nil {
		return err
	}
	tr, err := a.exchangeCode(ctx, code, verifier, redirectURI)
	if err != nil {
		return err
	}
	return a.storeTokens(tr)
}

// fetchAuthctx performs the /oauth/authorize GET without following the 302 and
// extracts the authctx from the Location header.
func (a *Authenticator) fetchAuthctx(ctx context.Context, challenge, state, redirectURI string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.authorizeURL(challenge, state, redirectURI), nil)
	if err != nil {
		return "", err
	}
	// Read the 302 instead of following it.
	noRedirect := *a.httpc
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", fmt.Errorf("authorize request: %w", err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("/oauth/authorize did not redirect (status %d)", resp.StatusCode)
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("parse authorize Location: %w", err)
	}
	authctx := u.Query().Get("authctx")
	if authctx == "" {
		return "", fmt.Errorf("authorize Location had no authctx: %s", loc)
	}
	return authctx, nil
}

// passwordExchange posts credentials to /authserver/password and extracts the
// authorization code from the returned redirectUri.
func (a *Authenticator) passwordExchange(ctx context.Context, authctx, email, password string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"authctx": authctx, "email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.env.CPBaseURL, "/")+"/authserver/password", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("password request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/authserver/password returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var pr struct {
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", fmt.Errorf("decode password response: %w", err)
	}
	u, err := url.Parse(pr.RedirectURI)
	if err != nil {
		return "", fmt.Errorf("parse redirectUri: %w", err)
	}
	code := u.Query().Get("code")
	if code == "" {
		return "", fmt.Errorf("password redirectUri had no code: %s", pr.RedirectURI)
	}
	return code, nil
}

// exchangeCode swaps an authorization code for tokens via /oauth/token.
func (a *Authenticator) exchangeCode(ctx context.Context, code, verifier, redirectURI string) (tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {a.env.OAuthClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.env.CPBaseURL, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("/oauth/token returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenResponse{}, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("token response had no access_token")
	}
	return tr, nil
}

// storeTokens persists the access and refresh tokens in the keychain.
func (a *Authenticator) storeTokens(tr tokenResponse) error {
	if err := a.store.Set(a.env.Name, SecretAccessToken, tr.AccessToken); err != nil {
		return err
	}
	if tr.RefreshToken != "" {
		if err := a.store.Set(a.env.Name, SecretRefreshToken, tr.RefreshToken); err != nil {
			return err
		}
	}
	return nil
}

// browserCommand returns the command and args that open target in the default
// browser on the given GOOS. Pure (no exec) so the platform mapping is unit
// testable without launching anything.
func browserCommand(goos, target string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{target}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		return "xdg-open", []string{target}
	}
}

// openInBrowser opens url in the OS default browser.
func openInBrowser(target string) error {
	cmd, args := browserCommand(runtime.GOOS, target)
	return exec.Command(cmd, args...).Start()
}
