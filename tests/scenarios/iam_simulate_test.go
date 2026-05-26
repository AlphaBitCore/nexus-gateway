// IAM family follow-on (S-113 — /iam/simulate parity from catalog
// §5.13 gap). The simulate endpoint is the "what would happen if I
// tried this?" tool admins use to debug 403 fall-throughs before
// publishing a policy. Parity means: simulate's decision must match
// what iamMW would return for the same (principal, action, resource).
// If they disagree, a policy that looks Allow in the simulator silently
// 403s in production — the canonical operator-trust failure for IAM.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS113_IAMSimulateParity — PM-grade e2e.
//
// BRAINSTORM (pre): two parity directions matter most.
//
//   1. Allow parity: super-admin should be Allowed for read on the
//      iam-policy resource (the simulate endpoint itself is gated by
//      iam:policy.read). We confirm the simulator agrees AND that a
//      real GET of the gated endpoint returns 200.
//   2. Deny parity: super-admin should be Denied for a deliberately
//      malformed action (`admin:nonexistent.action.fakeverb`). The
//      simulator must say deny — and any iamMW would also reject.
//
// Cross-service: CP-only. No Hub. PM-grade because the alternative
// (simulator only validates the bytes, not the semantics) means
// admins relying on "simulate first" silently get the wrong answer
// during policy authoring.
//
// Assertions:
//   1. Simulate (user/super-admin, iam:policy.read, nrn:iam:policy/_)
//      returns decision="Allow".
//   2. Live GET /api/admin/iam/policies returns 200 (proves the
//      simulator's allow is actually honoured by iamMW).
//   3. Simulate with a bogus action returns decision="Deny" or
//      empty (no policy matches).
func TestS113_IAMSimulateParity(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	const superAdminID = "nexus-user-super-admin"

	// (1) Allow path: super-admin reading iam-policy resource.
	allowBody, _ := json.Marshal(map[string]any{
		"principal": map[string]string{"type": "nexus_user", "id": superAdminID},
		"action":    "admin:iam-policy.read",
		"resource":  "nrn:nexus:iam-policy:_:_",
	})
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/iam/simulate", allowBody)
	if err != nil {
		t.Fatalf("simulate allow: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("simulate allow: status %d body=%q", status, truncate(body, 200))
	}
	var allow struct {
		Decision          string `json:"decision"`
		Reason            string `json:"reason"`
		MatchedStatements []any  `json:"matchedStatements"`
	}
	if err := json.Unmarshal(body, &allow); err != nil {
		t.Fatalf("decode allow: %v body=%q", err, truncate(body, 300))
	}
	if allow.Decision != "Allow" {
		t.Errorf("super-admin iam-policy.read: decision=%q want Allow (reason=%q matched=%d)",
			allow.Decision, allow.Reason, len(allow.MatchedStatements))
	}

	// (2) Live request must agree — iamMW honours the same Allow.
	liveStatus, liveBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/iam/policies?limit=1", nil)
	if err != nil {
		t.Fatalf("live policies GET: %v", err)
	}
	if liveStatus != http.StatusOK {
		t.Errorf("parity violation: simulate said Allow but live GET /iam/policies returned %d (body=%q)",
			liveStatus, truncate(liveBody, 200))
	}

	// (3) Deny path: must use a non-privileged principal. Super-admin has
	// a wildcard `admin:*` policy that would Allow even a bogus action,
	// so testing Deny against super-admin is uninformative. A non-existent
	// user id has no IamPolicyAttachment rows, so the engine returns
	// "No policies attached" → Deny — the canonical no-match outcome.
	const noPolicyUser = "s113-nonexistent-user"
	denyBody, _ := json.Marshal(map[string]any{
		"principal": map[string]string{"type": "nexus_user", "id": noPolicyUser},
		"action":    "admin:iam-policy.read",
		"resource":  "nrn:nexus:iam-policy:_:_",
	})
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/iam/simulate", denyBody)
	if err != nil {
		t.Fatalf("simulate deny: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("simulate deny: status %d body=%q", status, truncate(body, 200))
	}
	var deny struct {
		Decision          string `json:"decision"`
		MatchedStatements []any  `json:"matchedStatements"`
	}
	if err := json.Unmarshal(body, &deny); err != nil {
		t.Fatalf("decode deny: %v body=%q", err, truncate(body, 300))
	}
	if deny.Decision == "Allow" {
		t.Errorf("bogus action shouldn't be Allow: decision=%q matched=%d (engine should default to Deny on no-match)",
			deny.Decision, len(deny.MatchedStatements))
	}

	t.Logf("S-113 OK: simulate Allow=%v live=%d simulate Deny=%v",
		allow.Decision, liveStatus, deny.Decision)
}
