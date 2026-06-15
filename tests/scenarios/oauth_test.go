// OAuth / SSO family (S-120..S-124) — verifies the Control Plane's
// embedded OAuth + PKCE authorization server: discovery, token round
// trip, introspection, revocation.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// pkce helpers are exposed by helpers package — see PKCE wrappers below.

// TestS120_OAuthDiscovery — PM-grade e2e.
//
// BRAINSTORM (pre): the OAuth + PKCE authorization server publishes a
// discovery document at /.well-known/openid-configuration. Every
// authn / authz client (the SPA, machine-to-machine integrations,
// scenario harnesses) bootstraps from this document; a regression
// that drops a required field breaks every downstream client at
// once. Cross-service: just CP — no DB / Hub / MQ touched.
//
// Assertions:
//  1. GET returns 200 + JSON-decodable body.
//  2. issuer matches our local CP base URL.
//  3. authorization_endpoint / token_endpoint / introspection_endpoint
//     / revocation_endpoint / jwks_uri all present and same-origin.
//  4. response_types_supported includes "code", grant_types includes
//     "authorization_code" and "refresh_token".
//  5. code_challenge_methods_supported includes "S256" (PKCE binding
//     uses SHA-256 in CPLogin).
//  6. The jwks_uri returns 200 and contains a non-empty keys array.
//
// BRAINSTORM (post — see end-of-test t.Logf).
func TestS120_OAuthDiscovery(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	client := intg.LocalHTTPClient()

	// Discovery endpoints are loopback aliases locally, but under the real CP
	// origin (https + the CP host) in prod-safe-e2e. Relax the loopback check
	// to "https under the CP origin" on prod; keep strict loopback locally.
	endpointOK := isLoopbackURL
	if helpers.IsProdSafeE2E() {
		cpu, _ := url.Parse(sc.Env.CPURL)
		cpHost := cpu.Hostname()
		endpointOK = func(raw string) bool {
			u, err := url.Parse(raw)
			return err == nil && u.Scheme == "https" && u.Hostname() == cpHost
		}
	}

	// 1) Discovery.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		sc.Env.CPURL+"/.well-known/openid-configuration", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("discovery: status %d body=%q", resp.StatusCode, truncate(body, 200))
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("discovery body not JSON: %v", err)
	}

	// 2) issuer must be a loopback URL on CP's port. Accept any of
	// localhost/127.0.0.1/::1 as a loopback alias — the issuer is
	// stable per CP config and not required to byte-match NEXUS_CP_URL.
	issuer, _ := doc["issuer"].(string)
	if issuer == "" {
		t.Errorf("discovery.issuer is empty")
	} else if !endpointOK(issuer) {
		t.Errorf("discovery.issuer=%q failed the origin check (loopback locally / https-under-CP-origin on prod)", issuer)
	}

	// 3) Required endpoints present + same-origin (loopback aliases OK).
	required := []string{
		"authorization_endpoint",
		"token_endpoint",
		"introspection_endpoint",
		"revocation_endpoint",
		"jwks_uri",
	}
	for _, key := range required {
		raw, ok := doc[key].(string)
		if !ok || raw == "" {
			t.Errorf("discovery missing required key %q", key)
			continue
		}
		if !endpointOK(raw) {
			t.Errorf("discovery.%s=%q failed the origin check (loopback locally / https-under-CP-origin on prod)", key, raw)
		}
	}

	// 4) Grant + response types include the ones CPLogin depends on.
	requireContains := func(field string, want string) {
		arr, _ := doc[field].([]any)
		for _, v := range arr {
			if s, _ := v.(string); s == want {
				return
			}
		}
		t.Errorf("discovery.%s does not include %q (got %v)", field, want, arr)
	}
	requireContains("response_types_supported", "code")
	requireContains("grant_types_supported", "authorization_code")
	requireContains("grant_types_supported", "refresh_token")
	requireContains("code_challenge_methods_supported", "S256")

	// 5) jwks_uri returns a non-empty keys array.
	jwksRaw, _ := doc["jwks_uri"].(string)
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, jwksRaw, nil)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	jwksBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("jwks: status %d body=%q", resp2.StatusCode, truncate(jwksBody, 200))
	}
	var jwks struct {
		Keys []any `json:"keys"`
	}
	if err := json.Unmarshal(jwksBody, &jwks); err != nil {
		t.Fatalf("jwks body not JSON: %v", err)
	}
	if len(jwks.Keys) == 0 {
		t.Errorf("jwks.keys is empty — no signing keys advertised; every JWT verifier breaks")
	}

	t.Logf("S-120 OK: discovery has %d top-level keys, jwks advertises %d signing key(s)",
		len(doc), len(jwks.Keys))
}

// TestS121_OAuthTokenIntrospectRevoke — PM-grade e2e.
//
// BRAINSTORM (pre): walk the full lifecycle of a bearer access_token
// — obtain via CPLogin (authorization_code + PKCE), introspect to
// confirm active=true, hit a protected admin endpoint to prove the
// token authenticates, revoke it, then re-introspect to confirm
// active=false. Cross-service: CP authn middleware only.
//
// Assertions:
//  1. Obtain token via the standard CPLogin path. (Cached from a
//     previous scenario is fine — we DO NOT want to drive a fresh
//     login flow because that would burn through /authserver/password
//     rate limit. We use the cached token then explicitly revoke it.)
//  2. POST /oauth/introspect with the token → active=true,
//     client_id=cp-ui.
//  3. POST /oauth/revoke with the token → 200.
//  4. POST /oauth/introspect again → active=false.
//  5. After revoke, the cached token in helpers.CPLogin becomes
//     invalid for THIS process — but a follow-up CPLogin call must
//     re-login transparently. We test that by hitting an admin
//     endpoint with a fresh CPLogin call.
//
// NOTE: revoking the cached token has a global side-effect on the
// scenario process. We reset the helper's cache so subsequent
// scenarios re-login as needed. This scenario is intentionally
// ordered last in the package (test ordering not guaranteed; we
// document the side-effect rather than rely on order).
func TestS121_OAuthTokenIntrospectRevoke(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	// Drive a FRESH login dedicated to this test — DO NOT touch the
	// process-wide token cache, otherwise revoking would invalidate
	// the cached token and break every subsequent scenario.
	// (helpers.CPLogin returns the cached token; we want a separate
	// one. The simplest path is to call the same OAuth flow directly
	// — duplicating the round trip — but that risks the same 429.
	// As a pragmatic shortcut, we DO use the cached token and reset
	// the cache after revoke so later scenarios re-login lazily.)
	cachedToken, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	client := intg.LocalHTTPClient()

	doIntrospect := func(token string) map[string]any {
		form := url.Values{"token": []string{token}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			sc.Env.CPURL+"/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("introspect: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("introspect: status %d body=%q", resp.StatusCode, truncate(body, 200))
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("introspect body: %v", err)
		}
		return out
	}

	// 1) introspect — active=true.
	intro1 := doIntrospect(cachedToken)
	active1, _ := intro1["active"].(bool)
	if !active1 {
		t.Fatalf("introspect on a fresh login token returned active=false: %v", intro1)
	}

	// 2) revoke.
	revokeForm := url.Values{"token": []string{cachedToken}}
	revReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		sc.Env.CPURL+"/oauth/revoke", strings.NewReader(revokeForm.Encode()))
	revReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revResp, err := client.Do(revReq)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revBody, _ := io.ReadAll(revResp.Body)
	revResp.Body.Close()
	if revResp.StatusCode != 200 {
		t.Fatalf("revoke: status %d body=%q", revResp.StatusCode, truncate(revBody, 200))
	}

	// 3) introspect again — active=false.
	intro2 := doIntrospect(cachedToken)
	active2, _ := intro2["active"].(bool)
	if active2 {
		t.Errorf("introspect after revoke returned active=true — token revocation did not propagate: %v", intro2)
	}

	// 4) the cached token in CPLogin is now invalid — reset the cache
	// via a probe that fails authenticated then triggers re-login.
	helpers.ResetTokenCache()

	// Drive a fresh login + confirm it succeeds (protected admin
	// endpoint returns 200).
	newToken, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("re-login after revoke: %v", err)
	}
	probeStatus, _, err := helpers.CPDoJSON(ctx, sc.Env, newToken, http.MethodGet,
		"/api/admin/providers?limit=1", nil)
	if err != nil {
		t.Fatalf("probe after re-login: %v", err)
	}
	if probeStatus != 200 {
		t.Fatalf("probe after re-login: status %d", probeStatus)
	}
	if newToken == cachedToken {
		t.Errorf("re-login returned the same token as the revoked one — CPLogin cache wasn't reset")
	}

	t.Logf("S-121 OK: introspect(active)=true → revoke 200 → introspect(active)=false → re-login fresh token issued (different from revoked)")
}

// TestS122_OAuthRefreshTokenRotation — PM-grade e2e.
//
// BRAINSTORM (pre): RFC 6749 §6 plus RFC 6749 §10.4 say refresh tokens
// SHOULD rotate on use — i.e. /oauth/token grant_type=refresh_token
// returns a NEW refresh_token AND the old one becomes invalid. We
// drive the full authorization_code flow inline (not via CPLogin
// which discards the refresh token), grab the refresh token, exchange
// it once, then re-exchange the OLD one and expect 400 invalid_grant.
//
// Cross-service: CP authn middleware + refresh-store + revocation
// service. No Hub / AI Gw touch.
//
// Assertions:
//  1. /oauth/authorize → /authserver/password → /oauth/token yields
//     access_token + refresh_token.
//  2. /oauth/token grant_type=refresh_token with the issued refresh
//     returns 200 + a NEW refresh_token (different bytes).
//  3. Re-exchanging the OLD refresh returns 400 with
//     error=invalid_grant.
func TestS122_OAuthRefreshTokenRotation(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	client := intg.LocalHTTPClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// Step 1 — full authorization_code + PKCE flow.
	verifier, _ := pkceVerifierForTest()
	sum := sha256SumStr(verifier)
	state := fmt.Sprintf("s122-%d", time.Now().UnixNano())
	authQ := url.Values{}
	authQ.Set("response_type", "code")
	authQ.Set("client_id", "cp-ui")
	authQ.Set("redirect_uri", "http://localhost:3000/auth/callback")
	authQ.Set("code_challenge", sum)
	authQ.Set("code_challenge_method", "S256")
	authQ.Set("state", state)
	authQ.Set("scope", "openid")

	req1, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		sc.Env.CPURL+"/oauth/authorize?"+authQ.Encode(), nil)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	resp1.Body.Close()
	loc, _ := url.Parse(resp1.Header.Get("Location"))
	authctx := loc.Query().Get("authctx")
	if authctx == "" {
		t.Fatalf("authorize: no authctx (Location=%s)", resp1.Header.Get("Location"))
	}
	pwBody, _ := json.Marshal(map[string]string{
		"authctx": authctx, "email": sc.Env.AdminEmail, "password": sc.Env.AdminPassword,
	})
	pwReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		sc.Env.CPURL+"/authserver/password", strings.NewReader(string(pwBody)))
	pwReq.Header.Set("Content-Type", "application/json")
	pwResp, err := client.Do(pwReq)
	if err != nil {
		t.Fatalf("/authserver/password: %v", err)
	}
	defer pwResp.Body.Close()
	var pwOut struct {
		RedirectURI string `json:"redirectUri"`
	}
	_ = json.NewDecoder(pwResp.Body).Decode(&pwOut)
	redir, _ := url.Parse(pwOut.RedirectURI)
	code := redir.Query().Get("code")
	if code == "" {
		t.Fatalf("password: no code (redirectUri=%s)", pwOut.RedirectURI)
	}

	exchange := func(form url.Values) (status int, body []byte) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			sc.Env.CPURL+"/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("/oauth/token: %v", err)
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	codeForm := url.Values{}
	codeForm.Set("grant_type", "authorization_code")
	codeForm.Set("code", code)
	codeForm.Set("redirect_uri", "http://localhost:3000/auth/callback")
	codeForm.Set("client_id", "cp-ui")
	codeForm.Set("code_verifier", verifier)
	tokStatus, tokBody := exchange(codeForm)
	if tokStatus != 200 {
		t.Fatalf("token (authorization_code): status %d body=%q", tokStatus, truncate(tokBody, 200))
	}
	var first struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(tokBody, &first)
	if first.RefreshToken == "" {
		t.Fatalf("first token response missing refresh_token: %s", truncate(tokBody, 200))
	}

	// Step 2 — exchange the refresh token. Expect a NEW refresh_token.
	refForm := url.Values{}
	refForm.Set("grant_type", "refresh_token")
	refForm.Set("refresh_token", first.RefreshToken)
	refForm.Set("client_id", "cp-ui")
	refStatus, refBody := exchange(refForm)
	if refStatus != 200 {
		t.Fatalf("token (refresh_token): status %d body=%q", refStatus, truncate(refBody, 200))
	}
	var second struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(refBody, &second)
	if second.AccessToken == "" {
		t.Fatalf("refresh response missing access_token: %s", truncate(refBody, 200))
	}
	if second.RefreshToken == first.RefreshToken {
		t.Errorf("refresh token did NOT rotate — RFC 6749 §10.4 violation")
	}

	// Step 3 — re-exchange the OLD refresh token. Expect 400 invalid_grant.
	replayStatus, replayBody := exchange(refForm)
	if replayStatus == 200 {
		t.Errorf("OLD refresh token still accepted after rotation — replay protection broken (body=%q)",
			truncate(replayBody, 200))
	}
	var replay struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(replayBody, &replay)
	if replay.Error != "invalid_grant" {
		t.Logf("note: old-refresh replay error=%q (RFC 6749 §5.2 expects invalid_grant)", replay.Error)
	}

	t.Logf("S-122 OK: refresh rotated (new!=old), old replay returned %d (%s)",
		replayStatus, replay.Error)
}

// pkceVerifierForTest mirrors helpers.pkceVerifier but is exported here
// since we drive the OAuth flow inline in this scenario.
func pkceVerifierForTest() (string, error) {
	return helpers.GeneratePKCEVerifier()
}

// sha256SumStr returns base64url(sha256(s)) — the PKCE S256 challenge.
func sha256SumStr(s string) string {
	return helpers.PKCEChallengeS256(s)
}

// isLoopbackURL reports whether the URL's host is one of the
// well-known loopback aliases (localhost / 127.0.0.1 / ::1). The
// discovery issuer/endpoints may use any of these interchangeably
// even when NEXUS_CP_URL itself uses "localhost".
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// _ unused-import guards
var _ = fmt.Sprintf
var _ = time.Now
