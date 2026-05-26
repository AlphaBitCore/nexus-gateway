// SCIM family (S-070) — verifies the Control Plane's SCIM 2.0
// provisioning surface: admin-API IdP create, admin-API SCIM token
// mint, then end-to-end user provisioning + listing + revocation
// using the minted SCIM bearer.
package scenarios_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS070_SCIMTokenRoundTrip — PM-grade e2e for the SCIM provisioning
// surface end-to-end.
//
// BRAINSTORM (pre): SCIM 2.0 is the de-facto enterprise identity
// provisioning protocol (Okta, Azure AD, Google Workspace all speak
// it). The Nexus Control Plane exposes it at /scim/v2/* gated by a
// bearer token minted against an IdentityProvider row. This scenario
// exercises the full minimum chain an external IdP would walk on
// initial setup:
//
//	(1) admin creates an OIDC IdentityProvider via the admin API,
//	(2) admin mints a SCIM bearer token scoped to that IdP,
//	(3) external IdP calls /scim/v2/ServiceProviderConfig with the
//	    bearer to discover capabilities,
//	(4) external IdP POSTs a /scim/v2/Users to provision a new user,
//	(5) external IdP lists /scim/v2/Users and confirms the new user
//	    is visible,
//	(6) admin revokes the SCIM token; subsequent SCIM calls 401.
//
// Cross-service: CP only — admin API (/api/admin/identity-providers,
// /api/admin/identity-provider/:idpId/scim-tokens) + SCIM endpoints
// (/scim/v2/Users, /scim/v2/ServiceProviderConfig). DB rows hit:
// IdentityProvider, ScimToken, NexusUser, UserFederatedIdentity (via
// LinkUserToIdP). No Hub / AI Gw / Compliance Proxy touched.
//
// Assertions:
//  1. IdP create returns 201 + a non-empty id.
//  2. SCIM token mint returns 201 + a plain bearer token (shown once).
//  3. GET /scim/v2/ServiceProviderConfig returns 200 + valid SCIM
//     JSON containing schemas, patch, bulk, filter keys.
//  4. POST /scim/v2/Users returns 201 + id, userName, meta.resourceType=User.
//  5. GET /scim/v2/Users returns the just-created user in Resources.
//  6. After DELETE /scim-tokens/:tokenId, the next SCIM call returns 401.
//
// Graceful skip: if either the admin IdP create or the SCIM token mint
// returns 404 — i.e. the SCIM/IdP surface is not deployed in this
// environment — the scenario skips rather than fails. Local dev MUST
// have it on (routes.go wiring unconditionally registers when DB is
// non-nil), but the skip is preserved as a forward-compat guard.
func TestS070_SCIMTokenRoundTrip(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	nonce := fmt.Sprintf("%d", time.Now().UnixNano())

	// ─── Step 1 — create an OIDC IdentityProvider ────────────────────────────
	// Type validator requires lowercase "oidc"; OIDC config requires
	// non-empty issuer, clientId, redirectUri, and clientSecret on
	// create. Use fixture values — we never drive the OAuth flow
	// against this IdP, the row is only needed to scope the SCIM token.
	idpBody := mustMarshal(t, map[string]any{
		"name": "scim-test-idp-" + nonce,
		"type": "oidc",
		"config": map[string]any{
			"issuer":       "https://idp.test.invalid/oauth2/default",
			"clientId":     "nexus-scim-test-" + nonce,
			"clientSecret": "test-secret-" + nonce,
			"redirectUri":  "http://localhost:3000/auth/idp/callback",
		},
	})
	idpStatus, idpResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/identity-providers", idpBody)
	if err != nil {
		t.Fatalf("create IdP: %v", err)
	}
	if idpStatus == http.StatusNotFound {
		t.Fatalf("S-070 requires IdP admin surface; got %d body=%q — /api/admin/identity-providers must be registered",
			idpStatus, truncate(idpResp, 200))
	}
	if idpStatus != http.StatusCreated {
		t.Fatalf("create IdP: status %d body=%q", idpStatus, truncate(idpResp, 200))
	}
	var idpOut struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(idpResp, &idpOut); err != nil {
		t.Fatalf("decode IdP: %v body=%q", err, truncate(idpResp, 200))
	}
	if idpOut.ID == "" {
		t.Fatalf("create IdP: empty id in response %q", truncate(idpResp, 200))
	}
	idpID := idpOut.ID
	sc.Cleanup.Register("idp:"+idpID, func() error {
		// The SCIM-provisioned user (Step 4) is linked back to this
		// IdP via UserFederatedIdentity; the IdP delete handler 409s
		// when linked users exist unless force=true is passed to
		// cascade. We always cascade in cleanup to keep the test
		// hermetic across reruns.
		st, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
			http.MethodDelete, "/api/admin/identity-providers/"+idpID+"?force=true", nil)
		if err != nil {
			return err
		}
		if st >= 300 && st != http.StatusNotFound {
			return fmt.Errorf("delete IdP %s: status %d body=%q",
				idpID, st, truncate(body, 200))
		}
		return nil
	})

	// ─── Step 2 — mint a SCIM token scoped to that IdP ───────────────────────
	tokenName := "test-scim-token-" + nonce
	mintBody := mustMarshal(t, map[string]any{"name": tokenName})
	// Note: the SCIM token route uses SINGULAR "/identity-provider/", not
	// the plural form used by IdP CRUD — handler is registered at
	// /api/admin/identity-provider/:idpId/scim-tokens (see
	// identity_provider.go:74-76).
	mintPath := "/api/admin/identity-provider/" + idpID + "/scim-tokens"
	mintStatus, mintResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, mintPath, mintBody)
	if err != nil {
		t.Fatalf("mint SCIM token: %v", err)
	}
	if mintStatus == http.StatusNotFound {
		t.Fatalf("S-070 requires SCIM endpoints; got %d body=%q — /api/admin/identity-provider/:id/scim-tokens must be registered",
			mintStatus, truncate(mintResp, 200))
	}
	if mintStatus != http.StatusCreated {
		t.Fatalf("mint SCIM token: status %d body=%q",
			mintStatus, truncate(mintResp, 200))
	}
	var mintOut struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(mintResp, &mintOut); err != nil {
		t.Fatalf("decode SCIM token: %v body=%q", err, truncate(mintResp, 200))
	}
	if mintOut.Token == "" || mintOut.ID == "" {
		t.Fatalf("mint SCIM token: empty token or id in response %q",
			truncate(mintResp, 200))
	}
	scimBearer := mintOut.Token
	scimTokenID := mintOut.ID

	// Cleanup registers a revoke even if Step 6 already revoked — the
	// handler returns 404 on a missing token which we treat as benign.
	sc.Cleanup.Register("scim-token:"+scimTokenID, func() error {
		st, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
			http.MethodDelete, mintPath+"/"+scimTokenID, nil)
		if err != nil {
			return err
		}
		if st >= 300 && st != http.StatusNotFound {
			return fmt.Errorf("revoke SCIM token %s: status %d body=%q",
				scimTokenID, st, truncate(body, 200))
		}
		return nil
	})

	// scimDo issues a SCIM HTTP request with the SCIM bearer + SCIM
	// content/accept headers. CPDoJSON only handles admin JSON auth, so
	// we build the request directly via the shared local HTTP client.
	client := intg.LocalHTTPClient()
	scimDo := func(method, path string, body []byte) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method,
			sc.Env.CPURL+path, reader)
		if err != nil {
			t.Fatalf("build SCIM request %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+scimBearer)
		req.Header.Set("Accept", "application/scim+json")
		if body != nil {
			req.Header.Set("Content-Type", "application/scim+json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("SCIM %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, respBody
	}

	// ─── Step 3 — discover via /ServiceProviderConfig ────────────────────────
	spcStatus, spcBody := scimDo(http.MethodGet, "/scim/v2/ServiceProviderConfig", nil)
	if spcStatus != http.StatusOK {
		t.Fatalf("GET ServiceProviderConfig: status %d body=%q",
			spcStatus, truncate(spcBody, 200))
	}
	var spc map[string]any
	if err := json.Unmarshal(spcBody, &spc); err != nil {
		t.Fatalf("ServiceProviderConfig body not JSON: %v body=%q",
			err, truncate(spcBody, 200))
	}
	// RFC 7643 §5 mandates these top-level keys; every Okta/Azure AD
	// SCIM client probes for them on first connect.
	for _, key := range []string{"schemas", "patch", "bulk", "filter"} {
		if _, ok := spc[key]; !ok {
			t.Errorf("ServiceProviderConfig missing required key %q (got keys=%v)",
				key, scimMapKeys(spc))
		}
	}

	// ─── Step 4 — provision a SCIM user ──────────────────────────────────────
	userName := fmt.Sprintf("scim-test-%s@nexus.invalid", nonce)
	userReqBody := mustMarshal(t, map[string]any{
		"schemas":     []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName":    userName,
		"displayName": "SCIM Test User " + nonce,
		"active":      true,
		"externalId":  "ext-" + nonce,
		"name": map[string]any{
			"givenName":  "Scim",
			"familyName": "Tester-" + nonce,
		},
		"emails": []map[string]any{
			{"value": userName, "primary": true},
		},
	})
	createUserStatus, createUserBody := scimDo(http.MethodPost,
		"/scim/v2/Users", userReqBody)
	if createUserStatus != http.StatusCreated {
		t.Fatalf("POST /scim/v2/Users: status %d body=%q",
			createUserStatus, truncate(createUserBody, 200))
	}
	var createdUser struct {
		ID       string `json:"id"`
		UserName string `json:"userName"`
		Meta     struct {
			ResourceType string `json:"resourceType"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(createUserBody, &createdUser); err != nil {
		t.Fatalf("decode created SCIM user: %v body=%q",
			err, truncate(createUserBody, 200))
	}
	if createdUser.ID == "" {
		t.Errorf("SCIM user response missing id: %q", truncate(createUserBody, 200))
	}
	if createdUser.UserName != userName {
		t.Errorf("SCIM user.userName=%q want %q", createdUser.UserName, userName)
	}
	if createdUser.Meta.ResourceType != "User" {
		t.Errorf("SCIM user.meta.resourceType=%q want %q",
			createdUser.Meta.ResourceType, "User")
	}

	// Register cleanup BEFORE the listing assertion — if listing fails
	// the user still needs to be deprovisioned. DELETE on the SCIM
	// surface is a soft "suspend", not a hard delete, which is fine
	// for test isolation (the row is no longer "active" + bears a
	// unique userName so re-runs do not collide).
	//
	// IMPORTANT: Step 6 below revokes the SCIM bearer. Cleanup is LIFO,
	// so this user-cleanup actually runs AFTER Step 6 has already
	// revoked the token — meaning scimDo will return 401. We treat 401
	// as benign here (the user record is still "active" but with a
	// unique nonce userName, so re-runs do not collide; production
	// audit/IAM correctness is not affected by a soft-suspended test
	// row). The Cleanup wrapper logs the result either way.
	scimUserID := createdUser.ID
	sc.Cleanup.Register("scim-user:"+scimUserID, func() error {
		st, body := scimDo(http.MethodDelete, "/scim/v2/Users/"+scimUserID, nil)
		switch {
		case st == http.StatusNoContent, st == http.StatusOK,
			st == http.StatusNotFound, st == http.StatusUnauthorized:
			return nil
		default:
			return fmt.Errorf("delete SCIM user %s: status %d body=%q",
				scimUserID, st, truncate(body, 200))
		}
	})

	// ─── Step 5 — list SCIM users and confirm the new user is present ─────
	listStatus, listBody := scimDo(http.MethodGet, "/scim/v2/Users", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("GET /scim/v2/Users: status %d body=%q",
			listStatus, truncate(listBody, 200))
	}
	var listOut struct {
		Schemas      []string         `json:"schemas"`
		Resources    []map[string]any `json:"Resources"`
		TotalResults int              `json:"totalResults"`
	}
	if err := json.Unmarshal(listBody, &listOut); err != nil {
		t.Fatalf("decode list users: %v body=%q", err, truncate(listBody, 200))
	}
	foundCreated := false
	for _, r := range listOut.Resources {
		if id, _ := r["id"].(string); id == scimUserID {
			foundCreated = true
			break
		}
	}
	if !foundCreated {
		t.Errorf("just-created SCIM user %s not present in list response (total=%d, listed=%d)",
			scimUserID, listOut.TotalResults, len(listOut.Resources))
	}

	// ─── Step 6 — revoke the SCIM token and confirm 401 on next call ─────
	revStatus, revBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodDelete, mintPath+"/"+scimTokenID, nil)
	if err != nil {
		t.Fatalf("revoke SCIM token: %v", err)
	}
	if revStatus != http.StatusNoContent && revStatus != http.StatusOK {
		t.Fatalf("revoke SCIM token: status %d body=%q",
			revStatus, truncate(revBody, 200))
	}
	postRevokeStatus, postRevokeBody := scimDo(http.MethodGet,
		"/scim/v2/ServiceProviderConfig", nil)
	if postRevokeStatus != http.StatusUnauthorized {
		t.Errorf("SCIM call after revoke: status %d (want 401) body=%q",
			postRevokeStatus, truncate(postRevokeBody, 200))
	}

	t.Logf("S-070 OK: IdP=%s scim-token=%s scim-user=%s — provision OK, list OK, revoke→401 OK",
		idpID, scimTokenID, scimUserID)
}

// scimMapKeys returns the sorted top-level keys of a JSON-decoded map
// so error messages list what we got when an expected key is missing.
// Named with a scim- prefix to avoid colliding with node_overrides_test.go's
// differently-typed mapKeys.
func scimMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// O(n²) insertion sort is fine — these maps are small (≤20 keys).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

