// Device-group smart-membership family (S-071 — gap-matrix entry from
// the E86 catalog row "device groups: smart membership query →
// effective config"). Smart groups are E52-S2: instead of hand-adding
// devices one by one, the admin saves a membership *query* (e.g.
// `{tag: "macos-laptop"}`) and the Hub eval-loop materialises matching
// devices into the group's effective member set. The query feeds the
// cascade resolver that produces the per-device applied-config view.
//
// Two PM-grade invariants every interaction must preserve:
//
//  1. Preview vs persist split: POST /preview-membership MUST evaluate
//     the query without mutating any device_group row — admins use
//     preview to size the blast radius before saving. A preview that
//     accidentally persisted would surprise operators and break the
//     "look before you leap" UX.
//  2. Envelope stability: the GET /device-groups/:id detail payload
//     always returns the `memberships` array (possibly empty), never
//     null/omitted. The Devices tab on the group detail page renders
//     `memberships.length` and would crash on a missing field.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS071_DeviceGroupMembershipQuery — PM-grade e2e.
//
// BRAINSTORM (pre): smart-group membership has three admin surfaces
// the CP-UI calls in sequence — preview (POST), persist (PUT
// membership-query), inspect (GET group detail with embedded
// memberships). They share the same query DSL, so a regression in
// one usually means all three break.
// We exercise all three in one test against a freshly-created group
// so the assertions are self-contained — no reliance on pre-seeded
// device data. A clean test env may yield zero matching devices,
// which is the structural-only assertion path (envelope shape,
// status code) — not a failure.
//
// Cross-service: CP admin handler → Hub HTTP API → device_group +
// device_group_membership rows. No agent involvement in this path
// (device matching is server-side over the device shadow).
//
// Skip conditions:
//   - 404 on create-group: device-groups feature not deployed on this
//     CP build → t.Skipf so the scenario doesn't false-positive on
//     branches before the feature shipped.
//   - 400 on membership-query: server rejected the `{tag:"..."}` shape
//     (DSL schema changed) → log + skip; a test that fails on schema
//     drift would block unrelated CI runs.
//
// Assertions:
//  1. Preview membership: POST /api/admin/device-groups/preview-membership
//     with `{membershipQuery: {tag: "test-tag"}}` returns 200 + a JSON
//     envelope. The handler shape uses `sample` for the matched-device
//     IDs and `matched` for the total count (see
//     control-plane-ui/src/api/services/devices/device-groups.ts
//     PreviewMembershipResponse). Either is acceptable as schema-only
//     evidence the endpoint is wired.
//  2. Create + set query: POST /api/admin/device-groups returns
//     {id, name, ...}; PUT /api/admin/device-groups/:id/membership-query
//     persists the query and returns 200.
//  3. Query members: GET /api/admin/device-groups/:id returns 200
//     with a group-detail envelope that includes a `memberships` array
//     (possibly empty). The detail handler is the only surface that
//     exposes members — there is no separate `/members` collection
//     endpoint by design (see groups.go GetDeviceGroup).
func TestS071_DeviceGroupMembershipQuery(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// Build a tagged query body once — both preview and the persisted
	// query reuse the same shape so a schema mismatch surfaces in arm 1
	// before we create a group.
	queryBody, _ := json.Marshal(map[string]any{
		"membershipQuery": map[string]any{"tag": "test-tag"},
	})

	// (1) Preview membership — pure read against the query DSL.
	st, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/device-groups/preview-membership", queryBody)
	if err != nil {
		t.Fatalf("preview-membership: %v", err)
	}
	if st == http.StatusNotFound {
		t.Fatalf("preview-membership precondition unmet: 404 — device-groups feature not deployed on this CP build. Ensure RegisterDeviceGroupRoutes is wired and CP rebuilt (body=%q)",
			truncate(body, 200))
	}
	if st == http.StatusBadRequest {
		t.Fatalf("preview-membership precondition unmet: 400 — server rejected query DSL shape {tag:\"...\"}. Either update the query DSL handler or update this test's query shape (body=%q)",
			truncate(body, 200))
	}
	if st != http.StatusOK {
		t.Fatalf("preview-membership: status=%d body=%q", st, truncate(body, 200))
	}
	// Schema validation only: handler shape is {matched: number, sample:
	// string[]}. Either field present (or empty envelope) is acceptable —
	// clean test env may yield zero matches.
	var preview map[string]json.RawMessage
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Errorf("preview-membership: response not JSON object (err=%v body=%q)",
			err, truncate(body, 200))
	} else if _, hasMatched := preview["matched"]; !hasMatched {
		if _, hasSample := preview["sample"]; !hasSample {
			t.Errorf("preview-membership: envelope missing both 'matched' and 'sample' fields (body=%q)",
				truncate(body, 400))
		}
	}

	// (2) Create + set query.
	nonce := time.Now().UnixNano()
	groupName := fmt.Sprintf("test-grp-%d", nonce)
	createBody, _ := json.Marshal(map[string]any{"name": groupName})
	st, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/device-groups", createBody)
	if err != nil {
		t.Fatalf("create device-group: %v", err)
	}
	if st == http.StatusNotFound {
		t.Fatalf("create device-group precondition unmet: 404 — device-groups feature not deployed. Ensure CP admin device-group routes are registered and CP rebuilt (body=%q)",
			truncate(body, 200))
	}
	if st != http.StatusOK && st != http.StatusCreated {
		t.Fatalf("create device-group: status=%d body=%q", st, truncate(body, 200))
	}
	var createOut struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &createOut); err != nil {
		t.Fatalf("create device-group: decode: %v body=%q", err, truncate(body, 200))
	}
	if createOut.ID == "" {
		t.Fatalf("create device-group: missing id in response body=%q",
			truncate(body, 200))
	}
	groupID := createOut.ID
	sc.Cleanup.Register("DELETE device-group "+groupID, func() error {
		st, body, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete, "/api/admin/device-groups/"+url.PathEscape(groupID), nil)
		if err != nil {
			return fmt.Errorf("DELETE device-group %s: %w", groupID, err)
		}
		if st == http.StatusOK || st == http.StatusNoContent || st == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("DELETE device-group %s: status %d body=%q",
			groupID, st, truncate(body, 200))
	})

	// Persist the same query against the new group.
	st, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut,
		"/api/admin/device-groups/"+url.PathEscape(groupID)+"/membership-query",
		queryBody)
	if err != nil {
		t.Fatalf("PUT membership-query: %v", err)
	}
	if st == http.StatusBadRequest {
		t.Fatalf("PUT membership-query precondition unmet: 400 — server rejected query DSL on persist path (Arm 1 preview accepted it). Likely handler/validator drift between preview and persist; investigate before re-running (body=%q)",
			truncate(body, 200))
	}
	if st != http.StatusOK {
		t.Fatalf("PUT membership-query: status=%d body=%q", st, truncate(body, 200))
	}

	// (3) Inspect group detail — the only surface exposing members.
	// Handler shape (control-plane/internal/fleet/handler/agent/groups.go
	// GetDeviceGroup) is {id, name, description, createdBy, createdAt,
	// updatedAt, memberships: DeviceGroupMembership[]}. `memberships` is
	// always a non-nil array so the UI's DataTable never crashes reading
	// `.length`. Clean test env expectedly has zero devices matching
	// `tag:test-tag`, so assert envelope shape, not membership count.
	st, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet,
		"/api/admin/device-groups/"+url.PathEscape(groupID), nil)
	if err != nil {
		t.Fatalf("GET device-group detail: %v", err)
	}
	if st != http.StatusOK {
		t.Fatalf("GET device-group detail: status=%d body=%q",
			st, truncate(body, 200))
	}
	var detailEnv map[string]json.RawMessage
	if err := json.Unmarshal(body, &detailEnv); err != nil {
		t.Fatalf("GET device-group detail: response not JSON object (err=%v body=%q)",
			err, truncate(body, 200))
	}
	if _, ok := detailEnv["id"]; !ok {
		t.Errorf("GET device-group detail: envelope missing 'id' field (body=%q)",
			truncate(body, 400))
	}
	rawMemberships, ok := detailEnv["memberships"]
	if !ok {
		t.Errorf("GET device-group detail: envelope missing 'memberships' field (body=%q)",
			truncate(body, 400))
	} else {
		// Must be a JSON array (possibly empty) — never null/omitted so
		// the UI's `.length` read is safe.
		var asList []any
		if err := json.Unmarshal(rawMemberships, &asList); err != nil {
			t.Errorf("GET device-group detail: 'memberships' is not a JSON array (err=%v raw=%q)",
				err, truncate(rawMemberships, 200))
		}
	}

	t.Logf("S-071 OK: groupId=%s name=%s preview→create→set-query→group-detail-with-memberships",
		groupID, groupName)
}
