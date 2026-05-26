// IAM family (S-110..S-115) — verifies the Control Plane's IAM engine
// makes the correct allow/deny decisions for seeded principals against
// admin-API actions.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS110_IAMViewerCannotWrite — PM-grade e2e.
//
// BRAINSTORM (pre): the seeded "NexusViewer" managed group attaches a
// read-only policy to nexus-user-viewer (Diana). Plan §4 S-110 wants
// "user in NexusViewer group can read but not write — POST returns
// 403". Doing it via the actual HTTP path requires Diana's password
// (not in seed.ts), so we use the IAM simulate endpoint
// /api/admin/iam/simulate which DRY-RUNs the engine without
// authenticating as the principal. That tests the same EvaluatePolicy
// function the real iamMW middleware calls — proves the engine, not
// the HTTP wiring.
//
// Cross-service: CP /api/admin/iam/simulate (admin auth required) →
// in-memory iam.Engine.Evaluate → policy/attachment/group rows in DB.
//
// Assertions:
//   1. simulate(viewer, virtual-key:create, ...) → Decision="Deny"
//   2. simulate(viewer, virtual-key:read, ...)   → Decision="Allow"
//   3. simulate(super-admin, virtual-key:create) → Decision="Allow"
//
// Reason field is logged for traceability.
func TestS110_IAMViewerCannotWrite(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	simulate := func(principalID, action string) (string, string) {
		body, _ := json.Marshal(map[string]any{
			"principal": map[string]string{"type": "nexus_user", "id": principalID},
			"action":    action,
			"resource":  "nrn:nexus:iam::*:virtual-key/*",
			"context":   map[string]any{},
		})
		status, respBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
			http.MethodPost, "/api/admin/iam/simulate", body)
		if err != nil {
			t.Fatalf("simulate(%s, %s): %v", principalID, action, err)
		}
		if status != 200 {
			t.Fatalf("simulate(%s, %s): status %d body=%q",
				principalID, action, status, truncate(respBody, 200))
		}
		var out struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(respBody, &out); err != nil {
			t.Fatalf("decode: %v body=%q", err, truncate(respBody, 200))
		}
		return out.Decision, out.Reason
	}

	// IAM action format is "admin:<resource>.<verb>" per seed NRN
	// conventions and the NexusViewer policy ([admin:*.read]).
	cases := []struct {
		principal string
		action    string
		want      string
	}{
		{"nexus-user-viewer", "admin:virtual-key.create", "Deny"},
		{"nexus-user-viewer", "admin:virtual-key.read", "Allow"},
		{"nexus-user-super-admin", "admin:virtual-key.create", "Allow"},
		{"nexus-user-super-admin", "admin:virtual-key.read", "Allow"},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%s", c.principal, c.action), func(t *testing.T) {
			decision, reason := simulate(c.principal, c.action)
			if decision != c.want {
				t.Errorf("decision=%q (reason=%q); want %q", decision, reason, c.want)
			} else {
				t.Logf("decision=%s reason=%s", decision, reason)
			}
		})
	}
}
