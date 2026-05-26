// Hooks dry-run scenarios (catalog §5 gap — `POST /hooks/:id/dry-run`).
// The dry-run endpoint runs a hook against a caller-supplied sample
// body without producing a traffic_event, MQ message, or audit row.
// Operators use it to author hook configs before flipping them on.
// PM-grade because the wrong answer here (a hook that "passes" in
// dry-run but blocks in prod, or vice versa) destroys trust in the
// entire hook authoring workflow.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS027_HookDryRunContract — PM-grade e2e.
//
// BRAINSTORM (pre): the dry-run flow has two PM-grade invariants:
//
//   1. 404 on unknown hook id — operators must not see "success" for
//      a hook they actually deleted; better to fail loudly.
//   2. The response envelope MUST carry executionTimeMs + stage; the
//      authoring UI binds to both. A missing/zero stage means the
//      author can't tell whether they tested the request-phase or
//      response-phase config. A missing executionTimeMs hides the
//      perf budget signal.
//
// Side-effect freedom (no traffic_event, no audit row) is the third
// invariant; the test asserts AdminAuditLog row count does NOT
// increase for our test window.
//
// Cross-service: CP → AI Gateway (for builtin hooks) OR CP → webhook
// URL (for webhook hooks). We exercise a real seeded builtin hook
// (pii-outbound-scanner) so the path through forwardHookTest hits
// the live AI Gateway internal endpoint.
//
// Assertions:
//   1. POST /hooks/totally-not-real/dry-run → 404.
//   2. POST /hooks/:realId/dry-run with a benign sample body → 200
//      with envelope {executionTimeMs|output|error, stage}.
//   3. No new AdminAuditLog row for the hook id within the test
//      window (side-effect-free contract).
func TestS027_HookDryRunContract(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (1) 404 on unknown id.
	st, body, _ := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost,
		"/api/admin/hooks/totally-not-a-real-hook-id-s027/dry-run",
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if st != http.StatusNotFound {
		t.Errorf("dry-run unknown id: status=%d (want 404) body=%q",
			st, truncate(body, 200))
	}

	// (2) Real builtin hook id.
	var hookID string
	if err := sc.DB.QueryRow(ctx,
		`SELECT id FROM "HookConfig" WHERE name = 'pii-outbound-scanner' LIMIT 1`,
	).Scan(&hookID); err != nil || hookID == "" {
		t.Skipf("no pii-outbound-scanner hook seeded — skipping (err=%v)", err)
	}

	// Snapshot audit row count before; the dry-run path is side-effect
	// free, so the count must not increase by our action.
	var auditBefore int
	_ = sc.DB.QueryRow(ctx, `
		SELECT count(*) FROM "AdminAuditLog"
		WHERE "entityId" = $1
	`, hookID).Scan(&auditBefore)

	sample := []byte(`{"messages":[{"role":"user","content":"hello world, no PII here"}]}`)
	st, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost,
		"/api/admin/hooks/"+hookID+"/dry-run", sample)
	if err != nil {
		t.Fatalf("dry-run real hook: %v", err)
	}
	if st != http.StatusOK {
		t.Fatalf("dry-run real hook: status=%d body=%q",
			st, truncate(body, 300))
	}

	// Tolerant decode — the response carries either output OR error,
	// plus executionTimeMs + stage in both cases.
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("dry-run body not JSON: %v body=%q",
			err, truncate(body, 300))
	}
	if _, hasET := resp["executionTimeMs"]; !hasET {
		t.Errorf("envelope missing executionTimeMs (body=%q)", truncate(body, 300))
	}
	stage, _ := resp["stage"].(string)
	if stage == "" {
		t.Errorf("envelope missing or empty stage (body=%q)", truncate(body, 300))
	}
	hasOutput := resp["output"] != nil
	hasError := resp["error"] != nil
	if !hasOutput && !hasError {
		t.Errorf("envelope carries neither output nor error: %+v", resp)
	}

	// (3) No new AdminAuditLog row for the hook id.
	var auditAfter int
	_ = sc.DB.QueryRow(ctx, `
		SELECT count(*) FROM "AdminAuditLog"
		WHERE "entityId" = $1
	`, hookID).Scan(&auditAfter)
	if auditAfter > auditBefore {
		t.Errorf("dry-run wrote an AdminAuditLog row (before=%d after=%d) — side-effect-free contract violated",
			auditBefore, auditAfter)
	}

	t.Logf("S-027 OK: hook=%s stage=%s output?%v error?%v audit Δ=%d (must be 0)",
		hookID, stage, hasOutput, hasError, auditAfter-auditBefore)
}
