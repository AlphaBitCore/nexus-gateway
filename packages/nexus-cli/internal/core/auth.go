package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultRefreshSkew is how long before expiry the JWT source refreshes
// proactively, mirroring the 60 s safety margin used by tests/lib/auth.sh.
const defaultRefreshSkew = 60 * time.Second

// TokenSource yields the credential header for an admin-authed request,
// refreshing transparently when needed. The two implementations correspond to
// the two surfaces in adminauth.go: a human's auth-server JWT and a machine's
// admin API key.
type TokenSource interface {
	// Credential returns the header name and value to attach. It returns an
	// error wrapping ErrUnauthorized when no usable credential is available
	// (e.g. not logged in, or the token expired and refresh failed).
	Credential(ctx context.Context) (header, value string, err error)
}

// NewTokenSource selects the credential surface for env: an admin key when one
// is stored (machine profile), otherwise the JWT/refresh flow (human profile).
func NewTokenSource(env Env, store SecretStore, httpc *http.Client) TokenSource {
	if _, err := store.Get(env.Name, SecretAdminKey); err == nil {
		return &apiKeyTokenSource{env: env, store: store}
	}
	return &jwtTokenSource{env: env, store: store, httpc: httpc, now: time.Now, skew: defaultRefreshSkew}
}

// apiKeyTokenSource attaches the stored admin API key as x-admin-key.
type apiKeyTokenSource struct {
	env   Env
	store SecretStore
}

func (s *apiKeyTokenSource) Credential(context.Context) (string, string, error) {
	key, err := s.store.Get(s.env.Name, SecretAdminKey)
	if err != nil {
		return "", "", &APIError{kind: ErrUnauthorized, Message: "no admin key stored for env " + s.env.Name}
	}
	return "x-admin-key", key, nil
}

// jwtTokenSource attaches the auth-server JWT as a bearer token, refreshing via
// the OAuth refresh_token grant when the access token is near expiry.
type jwtTokenSource struct {
	env   Env
	store SecretStore
	httpc *http.Client
	now   func() time.Time
	skew  time.Duration
}

func (s *jwtTokenSource) Credential(ctx context.Context) (string, string, error) {
	tok, err := s.store.Get(s.env.Name, SecretAccessToken)
	if err != nil {
		return "", "", &APIError{kind: ErrUnauthorized, Message: "not logged in to env " + s.env.Name}
	}
	exp, expErr := jwtExpiry(tok)
	// Token still comfortably valid → use it as-is.
	if expErr == nil && s.now().Add(s.skew).Before(exp) {
		return "Authorization", "Bearer " + tok, nil
	}
	// Near expiry (or exp unreadable) → attempt a refresh.
	newTok, refErr := s.refresh(ctx)
	if refErr == nil {
		return "Authorization", "Bearer " + newTok, nil
	}
	// Refresh failed but the existing token has not actually expired yet → use
	// it rather than forcing a needless re-login.
	if expErr == nil && s.now().Before(exp) {
		return "Authorization", "Bearer " + tok, nil
	}
	return "", "", &APIError{kind: ErrUnauthorized, Message: "session expired and refresh failed (login required): " + refErr.Error()}
}

// refresh exchanges the stored refresh token for a new access token and
// persists the result. It returns the new access token.
func (s *jwtTokenSource) refresh(ctx context.Context) (string, error) {
	rt, err := s.store.Get(s.env.Name, SecretRefreshToken)
	if err != nil {
		return "", fmt.Errorf("no refresh token: %w", err)
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"client_id":     {s.env.OAuthClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.env.CPBaseURL, "/")+"/oauth/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode refresh response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("refresh response had no access_token")
	}
	if err := s.store.Set(s.env.Name, SecretAccessToken, tr.AccessToken); err != nil {
		return "", err
	}
	if tr.RefreshToken != "" {
		_ = s.store.Set(s.env.Name, SecretRefreshToken, tr.RefreshToken)
	}
	return tr.AccessToken, nil
}

// tokenResponse is the OAuth token endpoint payload.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}
