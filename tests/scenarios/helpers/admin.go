package helpers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// tokenCache is a process-wide token reused across scenarios so that
// running multiple scenarios in one `go test` invocation doesn't burn
// through CP's /authserver/password rate limit (each cp_login burst of
// > ~3 requests/second trips a 429 lockout). The cached token is
// refreshed on demand when expiry approaches.
var (
	tokenMu     sync.RWMutex
	cachedTok   string
	cachedTokAt time.Time // expiry — refresh 60 s before this
)

// ResetTokenCache invalidates the process-wide token cache so the next
// CPLogin call drives a fresh OAuth round trip. Useful for scenarios
// that revoke the current token and then need a new one. Idempotent.
func ResetTokenCache() {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cachedTok = ""
	cachedTokAt = time.Time{}
}

// CPLogin returns a bearer access_token for the seeded super-admin.
// Cached across scenarios in the same `go test` process — only the
// first call drives the actual OAuth+PKCE flow; subsequent calls hand
// back the cached token until it nears expiry.
//
// We refuse to talk to a non-local CP regardless of cached state — the
// hostname allowlist enforced in MustBeLocalTarget is the primary guard,
// but this additional check makes the function safe to call from a
// future place that might bypass TestMain.
func CPLogin(ctx context.Context, env *intg.Env) (string, error) {
	// Fast path: still-valid cached token.
	tokenMu.RLock()
	if cachedTok != "" && time.Now().Add(60*time.Second).Before(cachedTokAt) {
		t := cachedTok
		tokenMu.RUnlock()
		return t, nil
	}
	tokenMu.RUnlock()

	// Slow path: log in under write lock to coalesce concurrent
	// scenario starts onto one OAuth round-trip.
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if cachedTok != "" && time.Now().Add(60*time.Second).Before(cachedTokAt) {
		return cachedTok, nil
	}
	tok, exp, err := doCPLogin(ctx, env)
	if err != nil {
		return "", err
	}
	cachedTok = tok
	cachedTokAt = exp
	return tok, nil
}

// doCPLogin drives the OAuth + PKCE three-step flow end-to-end. The
// public CPLogin wraps this with a process-wide cache.
//
// The flow runs end-to-end (no shortcuts) on purpose — it doubles as a
// smoke for /oauth/authorize, /authserver/password, /oauth/token. A
// regression in any of those breaks every scenario.
func doCPLogin(ctx context.Context, env *intg.Env) (string, time.Time, error) {
	if err := assertLocalCP(env); err != nil {
		return "", time.Time{}, err
	}

	client := intg.LocalHTTPClient()
	// We need to inspect the 302 Location ourselves, not follow it.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// PKCE.
	verifier, err := pkceVerifier()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("pkce verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := fmt.Sprintf("scenario-login-%d", time.Now().UnixNano())

	// Step 1 — /oauth/authorize. Local IdP redirects to /login?authctx=...
	authQ := url.Values{}
	authQ.Set("response_type", "code")
	authQ.Set("client_id", env.OAuthClientID)
	authQ.Set("redirect_uri", env.OAuthRedirect)
	authQ.Set("code_challenge", challenge)
	authQ.Set("code_challenge_method", "S256")
	authQ.Set("state", state)
	authQ.Set("scope", "openid")

	authReq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		env.CPURL+"/oauth/authorize?"+authQ.Encode(), nil)
	authResp, err := client.Do(authReq)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("/oauth/authorize: %w", err)
	}
	authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound && authResp.StatusCode != http.StatusSeeOther {
		return "", time.Time{}, fmt.Errorf("/oauth/authorize: expected 302/303, got %d", authResp.StatusCode)
	}
	loc, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("/oauth/authorize: parse Location: %w", err)
	}
	authctx := loc.Query().Get("authctx")
	if authctx == "" {
		return "", time.Time{}, fmt.Errorf("/oauth/authorize: missing authctx in Location=%q", authResp.Header.Get("Location"))
	}

	// Step 2 — /authserver/password.
	pwBody, _ := json.Marshal(map[string]string{
		"authctx":  authctx,
		"email":    env.AdminEmail,
		"password": env.AdminPassword,
	})
	pwReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		env.CPURL+"/authserver/password", strings.NewReader(string(pwBody)))
	pwReq.Header.Set("Content-Type", "application/json")
	pwResp, err := client.Do(pwReq)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("/authserver/password: %w", err)
	}
	defer pwResp.Body.Close()
	if pwResp.StatusCode != http.StatusOK {
		buf := make([]byte, 4096)
		n, _ := pwResp.Body.Read(buf)
		return "", time.Time{}, fmt.Errorf("/authserver/password: status %d body=%q", pwResp.StatusCode, buf[:n])
	}
	var pwOut struct {
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.NewDecoder(pwResp.Body).Decode(&pwOut); err != nil {
		return "", time.Time{}, fmt.Errorf("/authserver/password: decode: %w", err)
	}
	redir, err := url.Parse(pwOut.RedirectURI)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("/authserver/password: parse redirectUri %q: %w", pwOut.RedirectURI, err)
	}
	code := redir.Query().Get("code")
	if code == "" {
		return "", time.Time{}, fmt.Errorf("/authserver/password: missing code in redirectUri %q", pwOut.RedirectURI)
	}

	// Step 3 — /oauth/token (authorization_code + PKCE).
	tokenForm := url.Values{}
	tokenForm.Set("grant_type", "authorization_code")
	tokenForm.Set("code", code)
	tokenForm.Set("redirect_uri", env.OAuthRedirect)
	tokenForm.Set("client_id", env.OAuthClientID)
	tokenForm.Set("code_verifier", verifier)
	tokReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		env.CPURL+"/oauth/token", strings.NewReader(tokenForm.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokResp, err := client.Do(tokReq)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("/oauth/token: %w", err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		buf := make([]byte, 4096)
		n, _ := tokResp.Body.Read(buf)
		return "", time.Time{}, fmt.Errorf("/oauth/token: status %d body=%q", tokResp.StatusCode, buf[:n])
	}
	var tokOut struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(tokResp.Body).Decode(&tokOut); err != nil {
		return "", time.Time{}, fmt.Errorf("/oauth/token: decode: %w", err)
	}
	if tokOut.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("/oauth/token: empty access_token")
	}
	ttl := tokOut.ExpiresIn
	if ttl <= 0 {
		ttl = 3600 // fallback if CP doesn't advertise expiry
	}
	return tokOut.AccessToken, time.Now().Add(time.Duration(ttl) * time.Second), nil
}

// pkceVerifier returns a 43+ char base64url-encoded random string suitable
// for the PKCE code_verifier per RFC 7636 §4.1. 33 bytes raw → 44 chars
// base64url without padding, well within the 43-128 char window.
func pkceVerifier() (string, error) {
	b := make([]byte, 33)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GeneratePKCEVerifier exports pkceVerifier for scenarios that drive the
// OAuth flow inline (e.g. S-122 refresh-token rotation).
func GeneratePKCEVerifier() (string, error) { return pkceVerifier() }

// PKCEChallengeS256 returns base64url(sha256(verifier)) — the S256
// challenge value RFC 7636 §4.2 demands.
func PKCEChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// assertLocalCP refuses to talk to a non-local CP even if MustBeLocalTarget
// was somehow skipped. Defence-in-depth for cp_login: never authenticate
// against production from the scenario harness — EXCEPT in prod safe-e2e mode,
// where logging in is required to run the curated read-only subset. Login is
// auth, not a data mutation; any subsequent mutating admin call is still
// hard-blocked at the CPDoJSON choke point (GuardProdSafeE2E).
func assertLocalCP(env *intg.Env) error {
	u, err := url.Parse(env.CPURL)
	if err != nil {
		return fmt.Errorf("cp_login refuses unparseable NEXUS_CP_URL %q: %w", env.CPURL, err)
	}
	host := u.Hostname()
	if _, ok := allowedHosts[host]; !ok {
		if prodSafeE2E {
			return nil
		}
		return fmt.Errorf("cp_login refuses non-local NEXUS_CP_URL host %q", host)
	}
	return nil
}

// CPDoJSON wraps DoJSON to call any /api/* path on the local CP with a
// bearer token. Returns (status, body, err).
func CPDoJSON(ctx context.Context, env *intg.Env, token, method, path string, body []byte) (int, []byte, error) {
	if err := GuardProdSafeE2E(method, path); err != nil {
		return 0, nil, err
	}
	client := intg.LocalHTTPClient()
	return intg.DoJSON(client, ctx, method, env.CPURL+path, "Bearer "+token, body)
}

// CPDoWithKey calls a CP admin endpoint using the x-admin-key header
// instead of a JWT bearer — exercises the personal API key auth path
// (the user-self-service flow's "use" step). Returns (status, body, err).
func CPDoWithKey(ctx context.Context, env *intg.Env, rawKey, method, path string) (int, []byte, error) {
	if err := GuardProdSafeE2E(method, path); err != nil {
		return 0, nil, err
	}
	client := intg.LocalHTTPClient()
	req, err := http.NewRequestWithContext(ctx, method, env.CPURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("x-admin-key", rawKey)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// CreatedVK is what CreateMyVK returns to a test — the server hands the
// plain key back exactly once; we keep it on this struct so tests can
// use it directly and the t.Cleanup hook can delete by ID later.
type CreatedVK struct {
	ID     string
	Name   string
	RawKey string
	Prefix string
}

// CreateMyVK creates a personal virtual key owned by the authenticated
// user via POST /api/my/virtual-keys. Returns the plain raw key, which
// is only delivered once — losing it means the VK is unusable. Scenarios
// must register a t.Cleanup to call DeleteMyVK so test VKs don't leak.
func CreateMyVK(ctx context.Context, env *intg.Env, token, name string) (*CreatedVK, error) {
	return CreateMyVKWith(ctx, env, token, name, nil)
}

// CreateMyVKWith is the extended-options variant of CreateMyVK. Scenario
// callers that need quota / budget / model-allowlist constraints on the
// VK pass them here. opts==nil yields the same shape as CreateMyVK.
type CreateMyVKOpts struct {
	RateLimitRpm   *int     // requests-per-minute throttle; nil = unlimited
	BudgetLimitUSD *float64 // cost cap (USD); nil = unlimited
}

func CreateMyVKWith(ctx context.Context, env *intg.Env, token, name string, opts *CreateMyVKOpts) (*CreatedVK, error) {
	payload := map[string]any{"name": name}
	if opts != nil {
		if opts.RateLimitRpm != nil {
			payload["rateLimitRpm"] = *opts.RateLimitRpm
		}
		if opts.BudgetLimitUSD != nil {
			payload["budgetLimitUsd"] = *opts.BudgetLimitUSD
		}
	}
	body, _ := json.Marshal(payload)
	status, respBody, err := CPDoJSON(ctx, env, token, http.MethodPost, "/api/my/virtual-keys", body)
	if err != nil {
		return nil, fmt.Errorf("/api/my/virtual-keys POST: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("/api/my/virtual-keys POST: status %d body=%q", status, string(respBody))
	}
	var out struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Key       string `json:"key"`
		KeyPrefix string `json:"keyPrefix"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("/api/my/virtual-keys POST: decode: %w (body=%q)", err, string(respBody))
	}
	if out.Key == "" || out.ID == "" {
		return nil, fmt.Errorf("/api/my/virtual-keys POST: missing id or key in response: %s", string(respBody))
	}
	return &CreatedVK{
		ID:     out.ID,
		Name:   out.Name,
		RawKey: out.Key,
		Prefix: out.KeyPrefix,
	}, nil
}

// CreatedProvider is the subset of provider fields scenario tests rely on
// after a successful POST /api/admin/providers — id for cleanup, name +
// adapterType + baseURL to assert the round-trip preserved input shape.
type CreatedProvider struct {
	ID          string
	Name        string
	AdapterType string
	BaseURL     string
}

// CreateProviderOpts is the minimal Provider create payload covered by
// scenario tests. Extra fields can be added without disturbing existing
// callers — keep the struct narrow (mirror admin_providers.go bind).
type CreateProviderOpts struct {
	Name        string // required
	DisplayName string
	BaseURL     string // required
	AdapterType string // required — must be in handler.ValidAdapterTypes
	Description string
	Region      string
	APIVersion  string
}

// CreateProvider POSTs /api/admin/providers. Returns CreatedProvider for
// the round-trip assertion, with the provider's ID available for
// downstream Get/Update/Delete + Cleanup.Register.
func CreateProvider(ctx context.Context, env *intg.Env, token string, opts CreateProviderOpts) (*CreatedProvider, error) {
	if opts.Name == "" || opts.BaseURL == "" || opts.AdapterType == "" {
		return nil, fmt.Errorf("CreateProvider: name, baseURL, adapterType are required")
	}
	payload := map[string]any{
		"name":        opts.Name,
		"displayName": opts.DisplayName,
		"description": opts.Description,
		"baseUrl":     opts.BaseURL,
		"adapterType": opts.AdapterType,
	}
	if opts.Region != "" {
		payload["region"] = opts.Region
	}
	if opts.APIVersion != "" {
		payload["apiVersion"] = opts.APIVersion
	}
	body, _ := json.Marshal(payload)
	status, respBody, err := CPDoJSON(ctx, env, token, http.MethodPost, "/api/admin/providers", body)
	if err != nil {
		return nil, fmt.Errorf("/api/admin/providers POST: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("/api/admin/providers POST: status %d body=%q", status, string(respBody))
	}
	var out struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		AdapterType string `json:"adapterType"`
		BaseURL     string `json:"baseUrl"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("/api/admin/providers POST: decode: %w (body=%q)", err, string(respBody))
	}
	if out.ID == "" {
		return nil, fmt.Errorf("/api/admin/providers POST: missing id (body=%q)", string(respBody))
	}
	return &CreatedProvider{
		ID:          out.ID,
		Name:        out.Name,
		AdapterType: out.AdapterType,
		BaseURL:     out.BaseURL,
	}, nil
}

// GetProvider GETs /api/admin/providers/:id and returns the parsed body.
func GetProvider(ctx context.Context, env *intg.Env, token, id string) (map[string]any, error) {
	status, body, err := CPDoJSON(ctx, env, token, http.MethodGet, "/api/admin/providers/"+id, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("/api/admin/providers/%s GET: status %d body=%q", id, status, string(body))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// DeleteProvider DELETEs /api/admin/providers/:id. Treats 404 as OK
// (idempotent cleanup).
func DeleteProvider(ctx context.Context, env *intg.Env, token, id string) error {
	status, body, err := CPDoJSON(ctx, env, token, http.MethodDelete, "/api/admin/providers/"+id, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("DELETE /api/admin/providers/%s: status %d body=%q", id, status, string(body))
}

// ProviderTestConnection POSTs /api/admin/providers/test-connection with
// the supplied probe payload. The endpoint forwards the request to the
// upstream provider; for scenarios we typically pass a deliberately
// unreachable baseURL and assert the endpoint returns a *structured*
// failure (any 4xx/5xx with a JSON error envelope) rather than crashing.
//
// Returns (status, body, err). err is non-nil only for transport-level
// failures.
func ProviderTestConnection(ctx context.Context, env *intg.Env, token, name, adapterType, baseURL, apiKey string) (int, []byte, error) {
	body, _ := json.Marshal(map[string]string{
		"name":        name,
		"adapterType": adapterType,
		"baseUrl":     baseURL,
		"apiKey":      apiKey,
	})
	return CPDoJSON(ctx, env, token, http.MethodPost, "/api/admin/providers/test-connection", body)
}

// ProviderModelLookup looks up a Provider by name and finds one of its
// Models by code. Returns (providerID, modelID, error). Used by routing
// scenarios that need a real (providerID, modelID) pair to construct a
// single-strategy rule config.
func ProviderModelLookup(ctx context.Context, env *intg.Env, token, providerName, modelCode string) (string, string, error) {
	// Find provider via list.
	status, body, err := CPDoJSON(ctx, env, token, http.MethodGet,
		"/api/admin/providers?q="+providerName, nil)
	if err != nil {
		return "", "", fmt.Errorf("list providers: %w", err)
	}
	if status != http.StatusOK {
		return "", "", fmt.Errorf("list providers: status %d body=%q", status, string(body))
	}
	var listResp struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return "", "", fmt.Errorf("decode list: %w", err)
	}
	var providerID string
	for _, p := range listResp.Data {
		if p.Name == providerName {
			providerID = p.ID
			break
		}
	}
	if providerID == "" {
		return "", "", fmt.Errorf("provider %q not found (got %d entries)", providerName, len(listResp.Data))
	}

	// GET provider/:id includes models inline.
	got, err := GetProvider(ctx, env, token, providerID)
	if err != nil {
		return "", "", fmt.Errorf("get provider %s: %w", providerID, err)
	}
	if got == nil {
		return "", "", fmt.Errorf("provider %s not found on GET", providerID)
	}
	models, ok := got["models"].([]any)
	if !ok {
		return "", "", fmt.Errorf("provider %s has no models array (got %T)", providerID, got["models"])
	}
	for _, raw := range models {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if code, _ := m["code"].(string); code == modelCode {
			id, _ := m["id"].(string)
			if id == "" {
				return "", "", fmt.Errorf("model %s has empty id", modelCode)
			}
			return providerID, id, nil
		}
	}
	return "", "", fmt.Errorf("model code %q not found under provider %s", modelCode, providerName)
}

// CreatedRoutingRule carries the fields scenario tests need post-create.
type CreatedRoutingRule struct {
	ID           string
	Name         string
	StrategyType string
}

// CreateRoutingRuleOpts is the narrow scenario surface for routing rule
// creation. Mirrors the relevant fields of admin_routing.go bind.
//
// NOTE on FallbackChain vs strategyType="fallback":
//   - FallbackChain (top-level) supplies inline recovery targets attached
//     to the rule. The engine treats this rule as a *primary* rule with
//     attached recovery — routing_rule_id stamps on traffic_event.
//   - strategyType="fallback" produces a rule the engine reclassifies
//     into the *recovery* pool (per resolver.go::ResolveTargets — see the
//     "Recovery from fallback rules" branch). That pool is consulted
//     *only* when the primary fails; the rule's ID is NOT stamped on the
//     plan as routing_rule_id. So for "primary 200 with chain configured"
//     scenarios, use FallbackChain on a single-strategy rule.
type CreateRoutingRuleOpts struct {
	Name            string          // required
	StrategyType    string          // required — "single", "fallback", "loadbalance", ...
	Config          json.RawMessage // required — strategy-specific
	MatchConditions json.RawMessage // optional — empty = catch-all
	FallbackChain   json.RawMessage // optional — inline recovery targets
	Priority        int
	PipelineStage   *int // optional — 0 = narrowing (policy), 1 = routing (default)
	Enabled         *bool
}

// CreateRoutingRule POSTs /api/admin/routing-rules. Returns CreatedRoutingRule
// with the rule's ID and name for cleanup + assertion.
func CreateRoutingRule(ctx context.Context, env *intg.Env, token string, opts CreateRoutingRuleOpts) (*CreatedRoutingRule, error) {
	if opts.Name == "" || opts.StrategyType == "" || len(opts.Config) == 0 {
		return nil, fmt.Errorf("CreateRoutingRule: name, strategyType, config are required")
	}
	payload := map[string]any{
		"name":         opts.Name,
		"strategyType": opts.StrategyType,
		"config":       opts.Config,
		"priority":     opts.Priority,
	}
	if len(opts.MatchConditions) > 0 {
		payload["matchConditions"] = opts.MatchConditions
	}
	if len(opts.FallbackChain) > 0 {
		payload["fallbackChain"] = opts.FallbackChain
	}
	if opts.PipelineStage != nil {
		payload["pipelineStage"] = *opts.PipelineStage
	}
	if opts.Enabled != nil {
		payload["enabled"] = *opts.Enabled
	}
	body, _ := json.Marshal(payload)
	status, respBody, err := CPDoJSON(ctx, env, token, http.MethodPost, "/api/admin/routing-rules", body)
	if err != nil {
		return nil, fmt.Errorf("/api/admin/routing-rules POST: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("/api/admin/routing-rules POST: status %d body=%q", status, string(respBody))
	}
	var out struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		StrategyType string `json:"strategyType"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode: %w (body=%q)", err, string(respBody))
	}
	if out.ID == "" {
		return nil, fmt.Errorf("missing id in response: %s", string(respBody))
	}
	return &CreatedRoutingRule{ID: out.ID, Name: out.Name, StrategyType: out.StrategyType}, nil
}

// GetPayloadCaptureConfig fetches the current payload-capture
// settings (system_metadata blob). Returns the raw response so
// callers can later restore the same values.
func GetPayloadCaptureConfig(ctx context.Context, env *intg.Env, token string) (map[string]any, error) {
	status, body, err := CPDoJSON(ctx, env, token, http.MethodGet,
		"/api/admin/settings/payload-capture", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET payload-capture: status %d body=%q", status, string(body))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// UpdatePayloadCaptureConfig PUTs the new settings. The handler
// invalidates the payload_capture shadow which propagates to
// ai-gateway + compliance-proxy.
func UpdatePayloadCaptureConfig(ctx context.Context, env *intg.Env, token string, opts map[string]any) error {
	body, _ := json.Marshal(opts)
	status, respBody, err := CPDoJSON(ctx, env, token, http.MethodPut,
		"/api/admin/settings/payload-capture", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("PUT payload-capture: status %d body=%q", status, string(respBody))
	}
	return nil
}

// CreatedAlertChannel is the post-create surface scenario tests use.
type CreatedAlertChannel struct {
	ID   string
	Name string
	Type string
}

// CreateAlertChannel POSTs /api/admin/alerts/channels. Returns the
// created channel's ID for downstream test-/delete-routing.
func CreateAlertChannel(ctx context.Context, env *intg.Env, token, name, channelType string, config map[string]any) (*CreatedAlertChannel, error) {
	if config == nil {
		config = map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{
		"name":        name,
		"type":        channelType,
		"enabled":     true,
		"severities":  []string{"info", "warning", "error"},
		"sourceTypes": []string{},
		"config":      config,
	})
	status, respBody, err := CPDoJSON(ctx, env, token, http.MethodPost,
		"/api/admin/alerts/channels", body)
	if err != nil {
		return nil, fmt.Errorf("POST /api/admin/alerts/channels: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("POST /api/admin/alerts/channels: status %d body=%q", status, string(respBody))
	}
	var out CreatedAlertChannel
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode: %w (body=%q)", err, string(respBody))
	}
	if out.ID == "" {
		return nil, fmt.Errorf("missing channel id in response: %s", string(respBody))
	}
	return &out, nil
}

// DeleteAlertChannel DELETEs /api/admin/alerts/channels/:id. Treats
// 404 as OK for idempotent cleanup.
func DeleteAlertChannel(ctx context.Context, env *intg.Env, token, id string) error {
	status, body, err := CPDoJSON(ctx, env, token, http.MethodDelete,
		"/api/admin/alerts/channels/"+id, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("DELETE channel %s: status %d body=%q", id, status, string(body))
}

// TestAlertChannel POSTs /api/admin/alerts/channels/:id/test which
// dispatches a synthetic alert + records a Dispatch row. Returns the
// raw HTTP status + body so scenarios can assert structure.
func TestAlertChannel(ctx context.Context, env *intg.Env, token, id string) (int, []byte, error) {
	return CPDoJSON(ctx, env, token, http.MethodPost,
		"/api/admin/alerts/channels/"+id+"/test", nil)
}

// PassthroughOpts is the narrow scenario-facing surface for E48
// passthrough writes. Mirrors the admin_passthrough.go bind.
type PassthroughOpts struct {
	Enabled         bool
	BypassHooks     bool
	BypassCache     bool
	BypassNormalize bool
	ExpiresAt       *time.Time
	Reason          string
}

// SetAdapterPassthrough writes the gateway_passthrough_config_adapter
// row for the given adapter type (PUT /api/admin/passthrough/adapter/:a).
// Adapter-scoped kill-switch limits blast radius — only that adapter's
// traffic bypasses hooks / cache / normalise. Returns the persisted
// response body so callers can inspect ExpiresAt server-rounded.
func SetAdapterPassthrough(ctx context.Context, env *intg.Env, token, adapterType string, opts PassthroughOpts) ([]byte, error) {
	payload := map[string]any{
		"enabled":         opts.Enabled,
		"bypassHooks":     opts.BypassHooks,
		"bypassCache":     opts.BypassCache,
		"bypassNormalize": opts.BypassNormalize,
		"reason":          opts.Reason,
	}
	if opts.ExpiresAt != nil {
		payload["expiresAt"] = opts.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	body, _ := json.Marshal(payload)
	status, respBody, err := CPDoJSON(ctx, env, token, http.MethodPut,
		"/api/admin/passthrough/adapter/"+adapterType, body)
	if err != nil {
		return nil, fmt.Errorf("PUT passthrough/adapter/%s: %w", adapterType, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("PUT passthrough/adapter/%s: status %d body=%q",
			adapterType, status, string(respBody))
	}
	return respBody, nil
}

// DisableAdapterPassthrough convenience to clear an adapter-scoped
// passthrough. Idempotent — PUT enabled=false even if no row exists.
func DisableAdapterPassthrough(ctx context.Context, env *intg.Env, token, adapterType string) error {
	_, err := SetAdapterPassthrough(ctx, env, token, adapterType, PassthroughOpts{
		Enabled: false,
		Reason:  "scenario cleanup",
	})
	return err
}

// DeleteRoutingRule removes a routing rule. 404 → no-op (idempotent cleanup).
func DeleteRoutingRule(ctx context.Context, env *intg.Env, token, ruleID string) error {
	status, body, err := CPDoJSON(ctx, env, token, http.MethodDelete,
		"/api/admin/routing-rules/"+ruleID, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("DELETE /api/admin/routing-rules/%s: status %d body=%q", ruleID, status, string(body))
}

// AdminAuditRow is the subset of the AdminAuditLog row scenario tests
// inspect. The integrity-hash chain columns aren't relevant to scenario
// assertions — only the actor/action/entity tuple is.
type AdminAuditRow struct {
	ID          string
	Action      string
	EntityType  string
	EntityID    string
	ActorID     string
	ActorLabel  string
	Timestamp   time.Time
	AfterState  string // JSONB as text
	BeforeState string
}

// WaitForAdminAuditRow polls the AdminAuditLog table until a row whose
// `action` AND `entityId` match the given values appears (or the
// deadline expires). The (action, entityId) tuple is the natural
// uniqueness key for CRUD admin operations — VirtualKey.create on
// a new VK ID is one row.
//
// Returns (nil, nil) on deadline — callers should branch on row == nil
// for "no audit row appeared in time".
func WaitForAdminAuditRow(
	ctx context.Context,
	db *pgxpool.Pool,
	action, entityID string,
	deadline time.Duration,
) (*AdminAuditRow, error) {
	stopAt := time.Now().Add(deadline)
	q := `
		SELECT id, action, "entityType", "entityId", "actorId", "actorLabel",
		       "timestamp",
		       COALESCE("afterState"::text, ''),
		       COALESCE("beforeState"::text, '')
		FROM "AdminAuditLog"
		WHERE "timestamp" > NOW() - INTERVAL '120 seconds'
		  AND action = $1
		  AND ($2 = '' OR "entityId" = $2)
		ORDER BY "timestamp" DESC LIMIT 1`
	for {
		var row AdminAuditRow
		err := db.QueryRow(ctx, q, action, entityID).Scan(
			&row.ID, &row.Action, &row.EntityType, &row.EntityID,
			&row.ActorID, &row.ActorLabel, &row.Timestamp,
			&row.AfterState, &row.BeforeState,
		)
		switch {
		case err == nil:
			return &row, nil
		case err == pgx.ErrNoRows:
			// retry
		default:
			return nil, fmt.Errorf("AdminAuditLog query: %w", err)
		}
		if time.Now().After(stopAt) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// DeleteMyVK removes a personal VK created by CreateMyVK. Suitable for
// passing to Cleanup.Register. We treat 404 as OK so a manually-cleaned
// VK doesn't make the cleanup phase noisy.
func DeleteMyVK(ctx context.Context, env *intg.Env, token, vkID string) error {
	status, body, err := CPDoJSON(ctx, env, token, http.MethodDelete,
		"/api/my/virtual-keys/"+vkID, nil)
	if err != nil {
		return fmt.Errorf("DELETE /api/my/virtual-keys/%s: %w", vkID, err)
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("DELETE /api/my/virtual-keys/%s: status %d body=%q", vkID, status, string(body))
}
