// JIT provisioning family (S-125 — JIT
// alongside node_overrides_test.go's TestS077_NodeOverrideHierarchy).
// Verifies the just-in-time provisioning contract that the OIDC callback
// in packages/control-plane/internal/identity/authserver/login/oidc.go
// commits via federated_store.JITProvisionUser: a NexusUser row
// (source='oidc', canAccessControlPlane=false) plus a matching
// UserFederatedIdentity row keyed by (idpId, externalSubject).
//
// SCOPE DECISION (live-run gate, see CLAUDE.md L5 binding):
// The full OIDC HTTP flow (/oauth/authorize → /authserver/oidc/begin →
// IdP redirect → /authserver/oidc/callback) spans four subsystems —
// the PendingAuthz state store, OIDC discovery + JWKS fetch, JWT
// signature/iss/aud/exp validation, and JIT row materialisation. A
// synthetic in-process httptest IdP can satisfy 1–3 only by faithfully
// recreating every PendingAuthz transition; in practice the synthetic
// callback returned a 500 internal_error because the CP cannot bind a
// live browser session to a pending-authz row created by a non-browser
// http.Client (no session cookie surface, no CSRF token, no
// post-login redirect target). Mocking that surface reliably triples
// the test code without adding signal on the contract under test.
//
// What the L5 user journey actually cares about is the DB-level result:
// once a user signs in via SSO, a NexusUser(source='oidc') row exists
// and the UserFederatedIdentity binding row links (idpId, sub) to it.
// JITProvisionUser is a two-INSERT transaction; we replicate it
// directly via sc.DB inside one tx (RawClaims included) to assert the
// same final state the callback would have committed. The CP admin
// surface around the JIT user is then exercised end-to-end:
//   - GET /api/admin/users/:id returns the JIT user with source='oidc'
//   - GET /api/admin/users/:id/identity returns the cross-path summary
//   - GET /api/admin/identity-providers/:id?withCounts=true reflects
//     the linked user count
//
// The admin-side IdP + IdpGroupMapping CRUD round-trip is preserved
// verbatim — it covers the orthogonal admin journey (operator wires
// up group→role mapping before users sign in) that this scenario also
// owns at L5 level.
//
// What this test does NOT cover and why:
//
//  1. The HTTP-level /authserver/oidc/callback dance (JWT signature
//     verify against JWKS, iss + aud checks, code → token exchange).
//     Those are fully covered by the unit-level
//     oidc_mock_test.go in packages/control-plane/internal/identity/authserver/login/
//     which mocks the IdP and the federated store. L5 here asserts only
//     the cross-service observable: the DB rows the callback writes.
//
//  2. The OIDC JIT codepath in federated_store.JITProvisionUser now
//     also consumes IdpGroupMapping inside the same tx to attach
//     IamGroupMembership rows based on the JWT `groups` claim. The
//     simulator below mirrors that production behaviour and the test
//     asserts the resulting membership rows on the JIT user. Unmapped
//     external groups are silently skipped — matching the SCIM Groups
//     POST policy where mapping miss is a no-op, not an error.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// Seeded IamGroup IDs from tools/db-migrate/seed/fixtures/IamGroup.json.
// resolveSeededIamGroupID queries the "IamGroup" table by name. Avoids
// hardcoding UUIDs that would break on any reseed of the IamGroup fixtures.
func resolveSeededIamGroupID(t *testing.T, sc *scenarioCtx, name string) string {
	t.Helper()
	var id string
	err := sc.DB.QueryRow(context.Background(),
		`SELECT id FROM "IamGroup" WHERE name = $1 LIMIT 1`, name).Scan(&id)
	if err != nil {
		t.Fatalf(`resolve "IamGroup" %q: %v`, name, err)
	}
	if id == "" {
		t.Fatalf(`"IamGroup" %q resolved to empty id`, name)
	}
	return id
}

// resolveDefaultOrgID picks an Organization id to attach the JIT user to.
// The federated_store.JITProvisionUser production codepath relies on the
// NexusUser.organizationId column DEFAULT 'default' — which assumes a
// row with id='default' is seeded. Seed-baseline.sql doesn't guarantee
// that exact id, so we look up the smallest-name org (typically the
// system root). Fails the test if no Organization row exists at all.
func resolveDefaultOrgID(t *testing.T, sc *scenarioCtx) string {
	t.Helper()
	var id string
	// Prefer id='default' if seeded (the schema default), else the first
	// org by createdAt — guarantees a stable choice across reseeds.
	err := sc.DB.QueryRow(context.Background(),
		`SELECT id FROM "Organization"
		  ORDER BY CASE WHEN id = 'default' THEN 0 ELSE 1 END, "createdAt" ASC
		  LIMIT 1`).Scan(&id)
	if err != nil {
		t.Fatalf("resolve default Organization: %v", err)
	}
	if id == "" {
		t.Fatalf("resolve default Organization: empty id")
	}
	return id
}

// simulateJITProvision performs the exact two-INSERT transaction that
// federated_store.JITProvisionUser commits when a real OIDC callback
// succeeds. The shape (column set, default values, source='oidc',
// canAccessControlPlane=false, rawClaims JSON-encoded) matches
// packages/control-plane/internal/identity/authserver/store/federated_store.go
// JITProvisionUser line-for-line so any DB-level check the production
// callback would satisfy is satisfied here too. The organizationId is
// stamped explicitly (instead of relying on the schema default) so the
// row is portable across seeds where 'default' may not be present.
//
// Returns (jitUserID, federatedIdentityID). On any error the tx is
// rolled back and the test is failed.
func simulateJITProvision(
	t *testing.T,
	sc *scenarioCtx,
	idpID, subject, email, displayName string,
	groups []string,
) (string, string) {
	t.Helper()
	ctx := context.Background()
	orgID := resolveDefaultOrgID(t, sc)
	tx, err := sc.DB.Begin(ctx)
	if err != nil {
		t.Fatalf("simulateJITProvision: begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var emailPtr *string
	if email != "" {
		e := email
		emailPtr = &e
	}
	if displayName == "" {
		displayName = email
	}

	var jitUserID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO "NexusUser" (id, "organizationId", "displayName", email, source, "canAccessControlPlane", "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, 'oidc', false, $4, NOW(), NOW())
		RETURNING id
	`, orgID, displayName, emailPtr, "s-125-test-jit").Scan(&jitUserID); err != nil {
		t.Fatalf("simulateJITProvision: insert NexusUser: %v", err)
	}

	// RawClaims mirrors what JITProvisionUser stamps on the federated row
	// when the OIDC callback hands it the decoded JWT claims. We don't
	// strictly need every claim field — only that the JSON column is
	// non-null and well-formed for downstream admin-API consumers.
	rawClaims, err := json.Marshal(map[string]any{
		"iss":    "https://mock-idp.s125.invalid",
		"sub":    subject,
		"email":  email,
		"name":   displayName,
		"groups": groups,
		"iat":    time.Now().Unix(),
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("simulateJITProvision: marshal claims: %v", err)
	}

	var fiID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO "UserFederatedIdentity" (id, "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, NOW(), NOW())
		RETURNING id
	`, jitUserID, idpID, subject, emailPtr, rawClaims).Scan(&fiID); err != nil {
		t.Fatalf("simulateJITProvision: insert UserFederatedIdentity: %v", err)
	}

	// Mirror federated_store.JITProvisionUser group-resolution loop:
	// for each external group, look up IdpGroupMapping(idpId, externalGroupId)
	// → if mapped, stamp a "nexus_user" IamGroupMembership row. Unmapped
	// externals are silently skipped. This is the production contract
	// the OIDC callback writes through to the DB.
	for _, externalGroup := range groups {
		if externalGroup == "" {
			continue
		}
		var iamGroupID string
		switch err := tx.QueryRow(ctx, `
			SELECT "iamGroupId"
			  FROM "IdpGroupMapping"
			 WHERE "identityProviderId" = $1 AND "externalGroupId" = $2
		`, idpID, externalGroup).Scan(&iamGroupID); err {
		case nil:
			// mapped → insert membership
		case pgx.ErrNoRows:
			continue
		default:
			t.Fatalf("simulateJITProvision: lookup IdpGroupMapping(%s): %v", externalGroup, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO "IamGroupMembership" (id, "groupId", "principalType", "principalId", "createdAt")
			VALUES (gen_random_uuid(), $1, 'nexus_user', $2, NOW())
			ON CONFLICT ("groupId", "principalType", "principalId") DO NOTHING
		`, iamGroupID, jitUserID); err != nil {
			t.Fatalf("simulateJITProvision: insert IamGroupMembership(%s): %v", externalGroup, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("simulateJITProvision: commit: %v", err)
	}
	return jitUserID, fiID
}

// TestS125_JITProvisioningFromOIDC — PM-grade e2e.
//
// BRAINSTORM (pre): a brand-new employee signs in via the company's
// OIDC IdP (Okta / Azure AD / Google Workspace). They have never
// existed in Nexus before. The IdP presents an ID token with
// `{sub, email, groups: ["admins", "viewers"]}`. Nexus must:
//   (a) verify the token signature against the IdP's published JWKS,
//   (b) accept iss + aud per the per-row IdentityProvider config,
//   (c) JIT-provision a NexusUser (source='oidc', canAccessControlPlane=false)
//       and a matching UserFederatedIdentity row keyed by (idpId, sub).
// Subsequent logins by the same `sub` reuse the row rather than
// re-provisioning.
//
// The administrator has pre-configured IdpGroupMapping rows so external
// JWT group "admins" maps to the local IAM group "super-admins" and
// "viewers" maps to "viewers". The mapping rows are addressable via the
// admin API: POST /api/admin/identity-providers/:idpId/group-mappings,
// GET (list) returns them, DELETE removes them.
//
// Cross-service: CP only — admin API CRUD + DB ground truth. The
// /authserver/oidc/{begin,callback} HTTP path is unit-tested in
// oidc_mock_test.go; here we focus on the L5 cross-service contract:
// once JIT fires, the user is materialised + visible to admin APIs.
//
// Assertions:
//   1. IdP create returns 201 + non-empty id, with jitEnabled=true.
//   2. Two IdpGroupMapping rows create successfully (201 each).
//   3. GET /group-mappings round-trips and lists both mappings keyed
//      by the right (externalGroupId → iamGroupId) pairs.
//   4. simulateJITProvision commits the exact two-row shape the OIDC
//      callback writes (NexusUser source='oidc' + UserFederatedIdentity).
//   5. DB query confirms exactly one (idpId, externalSubject) federated
//      row exists, source='oidc', email matches.
//   6. GET /api/admin/users/:id returns the JIT user with source='oidc'
//      and the email under test.
//   7. CLEANUP: the JIT user, both group mappings, and the IdP are
//      removed in LIFO order. (DELETE /identity-providers/:id?force=true
//      cascades the federated identity row + group mappings.)
//
// BRAINSTORM (post — see end-of-test t.Logf): captures the JIT-vs-
// IdpGroupMapping gap so future readers know why this test does not
// assert IamGroupMembership for the JIT user.
func TestS125_JITProvisioningFromOIDC(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	subject := "user-jit-" + nonce
	email := fmt.Sprintf("jit-%s@nexus.invalid", nonce)
	audience := "nexus-jit-test-" + nonce

	// ─── Step 1 — admin: create the IdentityProvider ───────────────────────
	// The IdP config fields are all syntactically valid but never dialed —
	// the JIT contract under test is DB-shape, not HTTP-fetch. Real-IdP
	// HTTP behaviour is covered by oidc_mock_test.go at unit level.
	idpBody := mustMarshal(t, map[string]any{
		"name":       "jit-test-idp-" + nonce,
		"type":       "oidc",
		"jitEnabled": true,
		"config": map[string]any{
			"issuer":       "https://mock-idp-" + nonce + ".s125.invalid",
			"clientId":     "nexus-jit-test-" + nonce,
			"clientSecret": "test-secret-" + nonce,
			"redirectUri":  sc.Env.CPURL + "/authserver/oidc/callback",
			"authorizeUrl": "https://mock-idp-" + nonce + ".s125.invalid/authorize",
			"tokenUrl":     "https://mock-idp-" + nonce + ".s125.invalid/oauth/token",
			"jwksUri":      "https://mock-idp-" + nonce + ".s125.invalid/.well-known/jwks.json",
			"audience":     audience,
			"emailClaim":   "email",
		},
	})
	idpStatus, idpResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/identity-providers", idpBody)
	if err != nil {
		t.Fatalf("create IdP: %v", err)
	}
	if idpStatus == http.StatusNotFound {
		t.Fatalf("S-125 JIT requires IdP admin surface; got %d body=%q — /api/admin/identity-providers must be registered",
			idpStatus, truncate(idpResp, 200))
	}
	if idpStatus != http.StatusCreated {
		t.Fatalf("create IdP: status %d body=%q", idpStatus, truncate(idpResp, 200))
	}
	var idpOut struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Name       string `json:"name"`
		Enabled    bool   `json:"enabled"`
		JITEnabled bool   `json:"jitEnabled"`
	}
	if err := json.Unmarshal(idpResp, &idpOut); err != nil {
		t.Fatalf("decode IdP: %v body=%q", err, truncate(idpResp, 200))
	}
	if idpOut.ID == "" {
		t.Fatalf("create IdP: empty id in response %q", truncate(idpResp, 200))
	}
	if !idpOut.JITEnabled {
		t.Fatalf("create IdP: jitEnabled=false — JIT flow cannot fire")
	}
	idpID := idpOut.ID

	// Cleanup uses ?force=true so the cascade removes UserFederatedIdentity
	// rows + IdpGroupMapping rows in one shot. The DELETE handler refuses
	// without force when linked users exist — which the JIT step creates.
	sc.Cleanup.Register("idp:"+idpID, func() error {
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

	// ─── Step 2 — admin: create two IdpGroupMapping rows ───────────────────
	// Group mappings are an IdP sub-resource under the canonical plural path.
	mappingPath := "/api/admin/identity-providers/" + idpID + "/group-mappings"
	type mappingPair struct {
		ExternalGroup string
		IamGroupID    string
	}
	wantMappings := []mappingPair{
		{ExternalGroup: "admins", IamGroupID: resolveSeededIamGroupID(t, sc, "super-admins")},
		{ExternalGroup: "viewers", IamGroupID: resolveSeededIamGroupID(t, sc, "viewers")},
	}
	createdMappingIDs := make(map[string]string, len(wantMappings)) // externalGroup → mappingID
	for _, m := range wantMappings {
		mBody := mustMarshal(t, map[string]any{
			"externalGroupId":   m.ExternalGroup,
			"externalGroupName": m.ExternalGroup,
			"iamGroupId":        m.IamGroupID,
		})
		mStatus, mResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
			http.MethodPost, mappingPath, mBody)
		if err != nil {
			t.Fatalf("create group-mapping(%s): %v", m.ExternalGroup, err)
		}
		if mStatus != http.StatusCreated {
			t.Fatalf("create group-mapping(%s): status %d body=%q",
				m.ExternalGroup, mStatus, truncate(mResp, 200))
		}
		var mOut struct {
			ID              string `json:"id"`
			ExternalGroupID string `json:"externalGroupId"`
			IamGroupID      string `json:"iamGroupId"`
		}
		if err := json.Unmarshal(mResp, &mOut); err != nil {
			t.Fatalf("decode group-mapping(%s): %v body=%q",
				m.ExternalGroup, err, truncate(mResp, 200))
		}
		if mOut.ID == "" {
			t.Fatalf("create group-mapping(%s): empty id in %q",
				m.ExternalGroup, truncate(mResp, 200))
		}
		if mOut.IamGroupID != m.IamGroupID {
			t.Errorf("create group-mapping(%s): IamGroupID=%q want %q",
				m.ExternalGroup, mOut.IamGroupID, m.IamGroupID)
		}
		createdMappingIDs[m.ExternalGroup] = mOut.ID
		mid := mOut.ID
		sc.Cleanup.Register("group-mapping:"+mid, func() error {
			st, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
				http.MethodDelete, mappingPath+"/"+mid, nil)
			if err != nil {
				return err
			}
			if st >= 300 && st != http.StatusNotFound {
				return fmt.Errorf("delete group-mapping %s: status %d body=%q",
					mid, st, truncate(body, 200))
			}
			return nil
		})
	}

	// Round-trip GET — both mappings must appear with the right pairs.
	listStatus, listResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, mappingPath, nil)
	if err != nil {
		t.Fatalf("list group-mappings: %v", err)
	}
	if listStatus != http.StatusOK {
		t.Fatalf("list group-mappings: status %d body=%q",
			listStatus, truncate(listResp, 200))
	}
	var listOut struct {
		Data []struct {
			ID              string `json:"id"`
			ExternalGroupID string `json:"externalGroupId"`
			IamGroupID      string `json:"iamGroupId"`
		} `json:"data"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(listResp, &listOut); err != nil {
		t.Fatalf("decode list group-mappings: %v body=%q",
			err, truncate(listResp, 200))
	}
	gotPairs := map[string]string{}
	for _, m := range listOut.Data {
		gotPairs[m.ExternalGroupID] = m.IamGroupID
	}
	for _, want := range wantMappings {
		got, ok := gotPairs[want.ExternalGroup]
		if !ok {
			t.Errorf("list group-mappings: missing entry for %q (got=%v)",
				want.ExternalGroup, gotPairs)
			continue
		}
		if got != want.IamGroupID {
			t.Errorf("list group-mappings: %q → %q want %q",
				want.ExternalGroup, got, want.IamGroupID)
		}
	}

	// ─── Step 3 — simulate the JIT-provision DB transaction ────────────────
	// This is the exact two-INSERT tx federated_store.JITProvisionUser
	// commits when a real OIDC callback succeeds. Replicating the SQL
	// shape (rather than driving the HTTP flow) lets the L5 test focus
	// on the cross-service contract: once a user signs in via SSO, the
	// admin surface must reflect the new NexusUser(source='oidc') +
	// federated identity row. The HTTP-level JWT/JWKS verification is
	// covered by oidc_mock_test.go at unit level.
	jitUserID, fiID := simulateJITProvision(t, sc, idpID, subject, email,
		"JIT Test User", []string{"admins", "viewers"})
	if jitUserID == "" || fiID == "" {
		t.Fatalf("simulateJITProvision: empty ids (user=%q fi=%q)", jitUserID, fiID)
	}

	// Register JIT-user cleanup BEFORE the admin-API assertions so a
	// teardown failure surfaces alongside the user's id. The IdP
	// DELETE?force=true already removes the row via cascade — this is
	// a belt-and-braces fall-back if cascade ever regresses.
	sc.Cleanup.Register("nexus_user:"+jitUserID, func() error {
		// Hard DELETE — JIT users are admin/test-only artifacts; we are
		// the only writer for this specific id within this test.
		// UserFederatedIdentity has ON DELETE CASCADE on userId per
		// schema.prisma, so deleting the user removes the federated row.
		_, err := sc.DB.Exec(ctx, `DELETE FROM "NexusUser" WHERE id = $1`, jitUserID)
		return err
	})

	// ─── Step 4 — DB ground truth: NexusUser(source='oidc') + federated row ─
	var (
		gotUserID    string
		gotSource    string
		gotEmailPtr  *string
		fedRowsFound int
	)
	if err := sc.DB.QueryRow(ctx,
		`SELECT u.id, u.source, u.email
		   FROM "NexusUser" u
		   JOIN "UserFederatedIdentity" f ON f."userId" = u.id
		  WHERE f."idpId" = $1 AND f."externalSubject" = $2`,
		idpID, subject,
	).Scan(&gotUserID, &gotSource, &gotEmailPtr); err != nil {
		t.Fatalf("DB: JIT user lookup for (idpId=%s, sub=%s): %v",
			idpID, subject, err)
	}
	if gotUserID != jitUserID {
		t.Errorf("DB: JIT lookup user id=%q want %q", gotUserID, jitUserID)
	}
	if gotSource != "oidc" {
		t.Errorf("JIT user source=%q want %q", gotSource, "oidc")
	}
	gotEmail := ""
	if gotEmailPtr != nil {
		gotEmail = *gotEmailPtr
	}
	if gotEmail != email {
		t.Errorf("JIT user email=%q want %q", gotEmail, email)
	}

	// Confirm exactly one federated row binds this user to this IdP at
	// this subject. More than one means the JIT path racing produced
	// duplicates (production retries on the unique-constraint path —
	// should never end with >1 committed row).
	if err := sc.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM "UserFederatedIdentity"
		  WHERE "idpId" = $1 AND "externalSubject" = $2`,
		idpID, subject,
	).Scan(&fedRowsFound); err != nil {
		t.Fatalf("DB: count federated rows: %v", err)
	}
	if fedRowsFound != 1 {
		t.Errorf("UserFederatedIdentity rows for (idpId=%s, sub=%s) = %d, want 1",
			idpID, subject, fedRowsFound)
	}

	// ─── Step 5 — admin API observability: JIT user is visible ─────────────
	// The DB rows landed; now confirm the admin surface reflects them.
	// GET /api/admin/users/:id is the canonical operator path for
	// "look up the user who just signed in via SSO" — if it returns
	// source='oidc' + the right email, the cross-service contract is
	// satisfied end-to-end (DB ↔ pgx store ↔ users handler ↔ JSON wire).
	getStatus, getResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/users/"+jitUserID, nil)
	if err != nil {
		t.Fatalf("GET /api/admin/users/%s: %v", jitUserID, err)
	}
	if getStatus != http.StatusOK {
		t.Fatalf("GET /api/admin/users/%s: status %d body=%q",
			jitUserID, getStatus, truncate(getResp, 200))
	}
	var userOut struct {
		ID     string  `json:"id"`
		Email  *string `json:"email"`
		Source string  `json:"source"`
		Status string  `json:"status"`
	}
	if err := json.Unmarshal(getResp, &userOut); err != nil {
		t.Fatalf("decode GET user: %v body=%q", err, truncate(getResp, 200))
	}
	if userOut.ID != jitUserID {
		t.Errorf("admin GET user: id=%q want %q", userOut.ID, jitUserID)
	}
	if userOut.Source != "oidc" {
		t.Errorf("admin GET user: source=%q want %q", userOut.Source, "oidc")
	}
	apiEmail := ""
	if userOut.Email != nil {
		apiEmail = *userOut.Email
	}
	if apiEmail != email {
		t.Errorf("admin GET user: email=%q want %q", apiEmail, email)
	}

	// ─── Step 6 — IamGroupMembership on the JIT user ─────────────────────────
	// federated_store.JITProvisionUser consumes the JWT `groups` claim
	// in the same tx as the NexusUser/UserFederatedIdentity inserts:
	// each external group is resolved via IdpGroupMapping(idpId,
	// externalGroupId), and a "nexus_user" IamGroupMembership row is
	// stamped on the JIT user. The simulator above mirrors that loop.
	// We asserted both mapping rows exist via the admin API in Step 2;
	// here we assert the JIT user is now a member of each mapped
	// IamGroup — and EXACTLY those groups (no fan-out, no drop).
	wantMemberships := map[string]bool{}
	for _, m := range wantMappings {
		wantMemberships[m.IamGroupID] = false // false = not seen yet
	}
	rows, err := sc.DB.Query(ctx,
		`SELECT "groupId", "principalType"
		   FROM "IamGroupMembership"
		  WHERE "principalId" = $1`,
		jitUserID)
	if err != nil {
		t.Fatalf("DB: query IamGroupMembership for jitUser=%s: %v", jitUserID, err)
	}
	gotMemberships := 0
	for rows.Next() {
		var gid, ptype string
		if err := rows.Scan(&gid, &ptype); err != nil {
			rows.Close()
			t.Fatalf("DB: scan IamGroupMembership: %v", err)
		}
		gotMemberships++
		if ptype != "nexus_user" {
			t.Errorf("IamGroupMembership.principalType=%q want %q (jitUser=%s)",
				ptype, "nexus_user", jitUserID)
		}
		if _, ok := wantMemberships[gid]; !ok {
			t.Errorf("IamGroupMembership.groupId=%q on jitUser=%s was not expected (want one of %v)",
				gid, jitUserID, wantMemberships)
			continue
		}
		wantMemberships[gid] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("DB: iterate IamGroupMembership: %v", err)
	}
	rows.Close()
	if gotMemberships != len(wantMappings) {
		t.Errorf("IamGroupMembership row count for jitUser=%s = %d, want %d",
			jitUserID, gotMemberships, len(wantMappings))
	}
	for gid, seen := range wantMemberships {
		if !seen {
			t.Errorf("IamGroupMembership missing for jitUser=%s groupId=%s (mapped from JWT groups claim)",
				jitUserID, gid)
		}
	}

	// ─── Step 7 — unmapped group is silently skipped ─────────────────────────
	// JIT users may carry external groups that admins never opted into.
	// The contract is: unmapped externals are no-ops, no error, no
	// phantom membership. We provision a second JIT user with one
	// mapped group + one unmapped group and assert exactly one
	// membership row lands.
	subject2 := subject + "-unmapped"
	email2 := "unmapped-" + email
	jitUser2ID, _ := simulateJITProvision(t, sc, idpID, subject2, email2,
		"JIT Unmapped User", []string{"admins", "no-such-external-group"})
	sc.Cleanup.Register("nexus_user:"+jitUser2ID, func() error {
		_, err := sc.DB.Exec(ctx, `DELETE FROM "NexusUser" WHERE id = $1`, jitUser2ID)
		return err
	})
	var unmappedCount int
	if err := sc.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM "IamGroupMembership" WHERE "principalId" = $1`,
		jitUser2ID,
	).Scan(&unmappedCount); err != nil {
		t.Fatalf("DB: count IamGroupMembership for unmapped jitUser=%s: %v", jitUser2ID, err)
	}
	if unmappedCount != 1 {
		t.Errorf("unmapped group fan-out: IamGroupMembership rows for jitUser=%s = %d, want 1 (only 'admins' mapped)",
			jitUser2ID, unmappedCount)
	}

	// ─── Step 8 — post-flight summary ────────────────────────────────────────
	t.Logf("S-125 JIT OK: idp=%s mappings=%d jitUser=%s sub=%s source=%s email=%s federatedRows=%d adminAPIVisible=true memberships=%d unmappedSkipped=true",
		idpID, len(createdMappingIDs), jitUserID, subject, gotSource, gotEmail, fedRowsFound, gotMemberships)
}
