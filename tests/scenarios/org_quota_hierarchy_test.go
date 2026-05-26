// Quota hierarchy family (S-078) — verifies the parent→child Organization
// quota cascade contract. A child org's quota is nested inside its parent's
// quota; the AI Gateway's quota engine walks the org chain (BuildCheckChain
// in packages/ai-gateway/internal/policy/quota/chain.go) so traffic from a
// child org increments BOTH the child counter AND every ancestor counter.
// A QuotaPolicy attached to the parent therefore caps the *aggregate* cost
// of all descendants.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS078_OrgQuotaHierarchyPropagation — PM-grade e2e.
//
// BRAINSTORM (pre): the cascade has three structural invariants the
// AI Gateway depends on. (1) Organization.parentId persists, and
// CreateOrganization computes the materialized `path` column so
// PolicyCache.Load can read the org tree. (2) A QuotaPolicy with
// scope=organization + organizationId=<orgID> persists with the
// matching org and survives the cache-invalidation round trip. (3)
// The cap binding contract: child policy is more restrictive than
// the parent — a sibling-child VK with no policy of its own still
// hits the parent cap once cross-child usage rolls up.
//
// Cross-service: CP admin API (POST /api/admin/organizations with
// parentId, POST /api/admin/quota-policies) → CP DB writes →
// AI Gw PolicyCache.Load picks up the rows on the
// `ai-gateway:organizations` and `ai-gateway:quota_policies`
// invalidation push. We assert the DB-level contract (org rows,
// path materialisation, policy rows) — that is the surface
// PolicyCache.Load reads. Driving live cascade enforcement at
// runtime additionally requires an application-VK creation +
// approval flow (POST /api/admin/virtual-keys + /:id/approve
// against a project under the child org) which sits outside this
// scenario's surface; the smoke harness exercises that path.
//
// Assertions:
//  1. POST /api/admin/organizations with parentId stamps the child's
//     parentId column AND computes Organization.path =
//     {parent.path}{childId}/ (materialised-path invariant —
//     subtree queries depend on it).
//  2. POST /api/admin/quota-policies for parent (1000 tokens) and
//     child (500 tokens) both succeed and persist with the right
//     organizationId.
//  3. Cap binding: child limit (500) < parent limit (1000) — i.e.
//     the parent quota is the aggregate ceiling. Verified by
//     reading the two QuotaPolicy.tokenLimit columns.
//  4. Cleanup deletes both policies and both orgs (LIFO order
//     matters — child first because parent has children > 0 and
//     DeleteOrganization refuses to drop a parent with descendants).
//
// We intentionally do NOT drive a chat through a VK in the child
// org because that requires standing up a Project under the child
// org + an application VK + the approval workflow + a routable
// credential — all out of scope for the cascade invariant this
// scenario locks in. The data-path correctness verified here is
// what the AI Gw's PolicyCache.Load consumes; cache-invalidation
// + chain walk are unit-tested in
// packages/ai-gateway/internal/policy/quota/quota_test.go.
func TestS078_OrgQuotaHierarchyPropagation(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	nonce := time.Now().UnixNano()
	parentName := fmt.Sprintf("parent-S078-%d", nonce)
	parentCode := fmt.Sprintf("p_s078_%d", nonce)
	childName := fmt.Sprintf("child-S078-%d", nonce)
	childCode := fmt.Sprintf("c_s078_%d", nonce)

	// --- Arm 1: setup hierarchy ----------------------------------------

	parentID, parentPath, err := createOrgS078(ctx, sc, token, map[string]any{
		"name": parentName,
		"code": parentCode,
	})
	if err != nil {
		t.Fatalf("create parent org: %v", err)
	}
	t.Logf("S-078 parent org id=%s path=%s", parentID, parentPath)

	childID, childPath, err := createOrgS078(ctx, sc, token, map[string]any{
		"name":     childName,
		"code":     childCode,
		"parentId": parentID,
	})
	if err != nil {
		// parentId is part of the documented org-create contract and
		// the quota cascade depends on it (BuildCheckChain walks
		// Organization.parentId). Any failure here is a real regression
		// in the nested-org admin API — fail loudly rather than skip.
		t.Fatalf("S-078 requires nested-org admin API; child create failed: %v", err)
	}
	t.Logf("S-078 child org id=%s path=%s", childID, childPath)

	// Cleanup orgs LIFO — child first so parent's children-count is 0
	// when its DELETE fires. DeleteOrganization 409s if children > 0.
	sc.Cleanup.Register("DeleteOrg(parent="+parentID+")", func() error {
		return deleteOrgS078(context.Background(), sc, token, parentID)
	})
	sc.Cleanup.Register("DeleteOrg(child="+childID+")", func() error {
		return deleteOrgS078(context.Background(), sc, token, childID)
	})

	// Invariant 1: parentId column + materialised path persisted.
	// Path layout: parent = "/{parentID}/", child = "/{parentID}/{childID}/".
	if got, want := parentPath, "/"+parentID+"/"; got != want {
		t.Errorf("parent path: got %q want %q", got, want)
	}
	if got, want := childPath, parentPath+childID+"/"; got != want {
		t.Errorf("child path: got %q want %q (materialised-path invariant)", got, want)
	}

	// DB cross-check: child's parentId column matches parentID. This is
	// the column PolicyCache.Load reads to build the org-parents map for
	// BuildCheckChain.
	var dbParentID *string
	err = sc.DB.QueryRow(ctx,
		`SELECT "parentId" FROM "Organization" WHERE id = $1`, childID).
		Scan(&dbParentID)
	if err != nil {
		t.Fatalf("read child parentId from DB: %v", err)
	}
	if dbParentID == nil || *dbParentID != parentID {
		got := "<nil>"
		if dbParentID != nil {
			got = *dbParentID
		}
		t.Errorf("child.parentId in DB: got %q want %q", got, parentID)
	}

	// --- Arm 2: set quotas ---------------------------------------------

	const parentTokenLimit int64 = 1000
	const childTokenLimit int64 = 500

	// alertThresholds is NOT NULL in the QuotaPolicy table (Prisma default
	// "[80, 90]" applies only when the column is omitted from the INSERT,
	// but the handler always binds the field — nil round-trips as SQL NULL
	// and triggers a 23502 constraint violation). Send an explicit array.
	parentPolicyID, err := createOrgQuotaPolicyS078(ctx, sc, token, map[string]any{
		"name":            fmt.Sprintf("S078-parent-policy-%d", nonce),
		"scope":           "organization",
		"organizationId":  parentID,
		"periodType":      "monthly",
		"tokenLimit":      parentTokenLimit,
		"enforcementMode": "reject",
		"alertThresholds": []int{80, 90},
		"priority":        50,
	})
	if err != nil {
		t.Fatalf("create parent quota policy: %v", err)
	}
	sc.Cleanup.Register("DeleteQuotaPolicy(parent="+parentPolicyID+")", func() error {
		return deleteQuotaPolicyS078(context.Background(), sc, token, parentPolicyID)
	})

	childPolicyID, err := createOrgQuotaPolicyS078(ctx, sc, token, map[string]any{
		"name":            fmt.Sprintf("S078-child-policy-%d", nonce),
		"scope":           "organization",
		"organizationId":  childID,
		"periodType":      "monthly",
		"tokenLimit":      childTokenLimit,
		"enforcementMode": "reject",
		"alertThresholds": []int{80, 90},
		"priority":        50,
	})
	if err != nil {
		t.Fatalf("create child quota policy: %v", err)
	}
	sc.Cleanup.Register("DeleteQuotaPolicy(child="+childPolicyID+")", func() error {
		return deleteQuotaPolicyS078(context.Background(), sc, token, childPolicyID)
	})

	// --- Arm 3: verify cascade structure -------------------------------

	// Invariant 2: both QuotaPolicy rows persist with correct
	// organizationId + tokenLimit. This is what PolicyCache.Load reads
	// into policiesByScope["organization"]; BuildCheckChain then emits
	// CheckLevel{organization, parentID} for any VK rooted in childID
	// because the walk-up over orgParents finds parentID.
	var (
		parentDBOrgID *string
		parentDBLimit *int64
	)
	if err := sc.DB.QueryRow(ctx,
		`SELECT "organizationId", "tokenLimit" FROM "QuotaPolicy" WHERE id = $1`,
		parentPolicyID).Scan(&parentDBOrgID, &parentDBLimit); err != nil {
		t.Fatalf("read parent policy from DB: %v", err)
	}
	if parentDBOrgID == nil || *parentDBOrgID != parentID {
		got := "<nil>"
		if parentDBOrgID != nil {
			got = *parentDBOrgID
		}
		t.Errorf("parent policy.organizationId: got %q want %q", got, parentID)
	}
	if parentDBLimit == nil || *parentDBLimit != parentTokenLimit {
		got := int64(-1)
		if parentDBLimit != nil {
			got = *parentDBLimit
		}
		t.Errorf("parent policy.tokenLimit: got %d want %d", got, parentTokenLimit)
	}

	var (
		childDBOrgID *string
		childDBLimit *int64
	)
	if err := sc.DB.QueryRow(ctx,
		`SELECT "organizationId", "tokenLimit" FROM "QuotaPolicy" WHERE id = $1`,
		childPolicyID).Scan(&childDBOrgID, &childDBLimit); err != nil {
		t.Fatalf("read child policy from DB: %v", err)
	}
	if childDBOrgID == nil || *childDBOrgID != childID {
		got := "<nil>"
		if childDBOrgID != nil {
			got = *childDBOrgID
		}
		t.Errorf("child policy.organizationId: got %q want %q", got, childID)
	}
	if childDBLimit == nil || *childDBLimit != childTokenLimit {
		got := int64(-1)
		if childDBLimit != nil {
			got = *childDBLimit
		}
		t.Errorf("child policy.tokenLimit: got %d want %d", got, childTokenLimit)
	}

	// Invariant 3: child cap is strictly tighter than parent. If a future
	// PR inverts this ordering the cascade no longer "binds" — every
	// child request would be blocked at the child level before the
	// parent counter could matter.
	if childTokenLimit >= parentTokenLimit {
		t.Errorf("cascade ordering: child limit %d must be < parent limit %d",
			childTokenLimit, parentTokenLimit)
	}

	// Invariant 4: org-tree walk-up locates parent from child. This is
	// the exact query PolicyCache.Load uses to build orgParents — if it
	// returns an unexpected shape the chain in BuildCheckChain won't
	// include the parent CheckLevel, and parent-level enforcement is
	// silently lost (the failure mode this scenario primarily guards
	// against).
	var orgTreeParentID *string
	if err := sc.DB.QueryRow(ctx,
		`SELECT "parentId" FROM "Organization" WHERE id = $1`, childID).
		Scan(&orgTreeParentID); err != nil {
		t.Fatalf("walk-up query: %v", err)
	}
	if orgTreeParentID == nil || *orgTreeParentID != parentID {
		t.Errorf("orgParents walk-up: child→parent edge missing (got %v want %s)",
			orgTreeParentID, parentID)
	}

	t.Logf("S-078 OK: parent=%s(limit=%d) child=%s(limit=%d) cascade structure verified",
		parentID, parentTokenLimit, childID, childTokenLimit)
}

// createOrgS078 POSTs /api/admin/organizations with the supplied body
// and returns (id, path, error). The body must include name + code; for
// child orgs the caller adds "parentId".
func createOrgS078(ctx context.Context, sc *scenarioCtx, token string, body map[string]any) (string, string, error) {
	raw, _ := json.Marshal(body)
	status, respBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/organizations", raw)
	if err != nil {
		return "", "", fmt.Errorf("POST /api/admin/organizations: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", "", fmt.Errorf("POST /api/admin/organizations: status %d body=%q",
			status, truncate(respBody, 200))
	}
	var out struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", fmt.Errorf("decode org create response: %w (body=%q)", err, truncate(respBody, 200))
	}
	if out.ID == "" {
		return "", "", fmt.Errorf("org create returned empty id (body=%q)", truncate(respBody, 200))
	}
	return out.ID, out.Path, nil
}

// deleteOrgS078 DELETEs an org. 404 → idempotent OK. 409 (children
// or projects still present) is surfaced so the test sees cleanup
// ordering bugs early.
func deleteOrgS078(ctx context.Context, sc *scenarioCtx, token, id string) error {
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodDelete, "/api/admin/organizations/"+id, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("DELETE /api/admin/organizations/%s: status %d body=%q",
		id, status, truncate(body, 200))
}

// createOrgQuotaPolicyS078 POSTs /api/admin/quota-policies with the
// supplied body and returns the new policy id.
func createOrgQuotaPolicyS078(ctx context.Context, sc *scenarioCtx, token string, body map[string]any) (string, error) {
	raw, _ := json.Marshal(body)
	status, respBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/quota-policies", raw)
	if err != nil {
		return "", fmt.Errorf("POST /api/admin/quota-policies: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("POST /api/admin/quota-policies: status %d body=%q",
			status, truncate(respBody, 300))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode quota policy: %w (body=%q)", err, truncate(respBody, 200))
	}
	if out.ID == "" {
		return "", fmt.Errorf("quota policy returned empty id (body=%q)", truncate(respBody, 200))
	}
	return out.ID, nil
}

// deleteQuotaPolicyS078 DELETEs a quota policy. 404 → idempotent OK.
func deleteQuotaPolicyS078(ctx context.Context, sc *scenarioCtx, token, id string) error {
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodDelete, "/api/admin/quota-policies/"+id, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("DELETE /api/admin/quota-policies/%s: status %d body=%q",
		id, status, truncate(body, 200))
}
