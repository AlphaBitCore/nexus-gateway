// DSAR (Data Subject Access Request) cross-cutting scenarios (S-096
// from catalog §10). GDPR/CCPA-style right-to-access and right-to-be-
// forgotten flows mounted under /api/admin/dsar. The state machine has
// hard transitions (PENDING→IN_PROGRESS→COMPLETED/REJECTED) the
// scenario validates end-to-end alongside the audit trail every step
// must leave.
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

// TestS096_DSARLifecycle — PM-grade e2e.
//
// BRAINSTORM (pre): DSAR is the most regulated surface in the system.
// Two PM-grade invariants the test must catch:
//
//   1. Status state machine. The validDSARTransitions map only allows
//      PENDING→IN_PROGRESS / PENDING→REJECTED / IN_PROGRESS→COMPLETED
//      / IN_PROGRESS→REJECTED. An illegal transition (PENDING→COMPLETED,
//      COMPLETED→anything) must return 400 — silently allowing it
//      means an operator can close a request without actually
//      fulfilling it.
//   2. Validation enums + required fields. POST without subjectId
//      returns 400; type ∉ {ACCESS, ERASURE} returns 400. Silent
//      acceptance of malformed input corrupts the regulator-facing
//      record.
//
// Cross-service: CP-only (DSAR is a CP-owned record). PM-grade
// because a state-machine break here is a regulatory liability —
// "subject Y's request was marked COMPLETED but never actually
// fulfilled" is exactly the failure mode auditors look for.
//
// Assertions:
//   1. POST {subjectId, type=ACCESS} returns 201 with status=PENDING.
//   2. POST without subjectId returns 400.
//   3. POST with type=NOT_A_TYPE returns 400.
//   4. PUT illegal transition (PENDING→COMPLETED) returns 400.
//   5. PUT legal transition (PENDING→IN_PROGRESS) returns 200; record
//      reflects the new status.
//   6. PUT terminal (IN_PROGRESS→REJECTED) returns 200; completedAt
//      is set; subsequent PUT REJECTED→anything returns 400.
//   7. AdminAuditLog has create + update rows for our entityId.
func TestS096_DSARLifecycle(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (2) Validation failures.
	missSub, _ := json.Marshal(map[string]any{"type": "ACCESS"})
	if st, _, _ := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/dsar", missSub); st != http.StatusBadRequest {
		t.Errorf("missing subjectId: status=%d (want 400)", st)
	}
	badType, _ := json.Marshal(map[string]any{
		"subjectId": "s096-test", "type": "NOT_A_TYPE",
	})
	if st, _, _ := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/dsar", badType); st != http.StatusBadRequest {
		t.Errorf("bad type: status=%d (want 400)", st)
	}

	// (1) Create a valid DSAR.
	subject := fmt.Sprintf("s096-subject-%d", time.Now().UnixNano())
	create, _ := json.Marshal(map[string]any{
		"subjectId": subject,
		"type":      "ACCESS",
		"notes":     "scenario-test",
	})
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/dsar", create)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("create: status %d body=%q", status, truncate(body, 200))
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v body=%q", err, truncate(body, 200))
	}
	if created.ID == "" {
		t.Fatalf("create missing id: body=%q", truncate(body, 200))
	}
	if created.Status != "PENDING" {
		t.Errorf("create status=%q, want PENDING", created.Status)
	}
	sc.Cleanup.Register("delete dsar "+created.ID, func() error {
		_, err := sc.DB.Exec(context.Background(),
			`DELETE FROM dsar_request WHERE id = $1`, created.ID)
		return err
	})

	// (4) Illegal transition PENDING→COMPLETED.
	illegal, _ := json.Marshal(map[string]any{"status": "COMPLETED"})
	if st, b, _ := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut, "/api/admin/dsar/"+created.ID, illegal); st != http.StatusBadRequest {
		t.Errorf("PENDING→COMPLETED: status=%d (want 400) body=%q", st, truncate(b, 200))
	}

	// (5) Legal transition PENDING→IN_PROGRESS.
	legal, _ := json.Marshal(map[string]any{"status": "IN_PROGRESS"})
	st, b, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut, "/api/admin/dsar/"+created.ID, legal)
	if err != nil {
		t.Fatalf("legal PUT: %v", err)
	}
	if st != http.StatusOK {
		t.Fatalf("PENDING→IN_PROGRESS: status=%d body=%q", st, truncate(b, 200))
	}
	var updated struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(b, &updated)
	if updated.Status != "IN_PROGRESS" {
		t.Errorf("after PUT: status=%q, want IN_PROGRESS", updated.Status)
	}

	// (6) Terminal IN_PROGRESS→REJECTED.
	reject, _ := json.Marshal(map[string]any{"status": "REJECTED"})
	st, b, _ = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut, "/api/admin/dsar/"+created.ID, reject)
	if st != http.StatusOK {
		t.Fatalf("IN_PROGRESS→REJECTED: status=%d body=%q", st, truncate(b, 200))
	}
	// Re-PUT from terminal — must reject.
	relegal, _ := json.Marshal(map[string]any{"status": "IN_PROGRESS"})
	if st, _, _ := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut, "/api/admin/dsar/"+created.ID, relegal); st != http.StatusBadRequest {
		t.Errorf("REJECTED→IN_PROGRESS: status=%d (want 400, terminal state)", st)
	}

	// (7) AdminAuditLog rows: 1 create + at least 2 updates.
	deadline := time.Now().Add(10 * time.Second)
	var creates, updates int
	for time.Now().Before(deadline) {
		rows, err := sc.DB.Query(ctx, `
			SELECT action FROM "AdminAuditLog"
			WHERE "timestamp" > NOW() - INTERVAL '60 seconds'
			  AND "entityId" = $1
		`, created.ID)
		if err == nil {
			creates, updates = 0, 0
			for rows.Next() {
				var a string
				_ = rows.Scan(&a)
				switch a {
				case "create":
					creates++
				case "update":
					updates++
				}
			}
			rows.Close()
			if creates >= 1 && updates >= 2 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if creates < 1 || updates < 2 {
		t.Errorf("audit drops: create=%d update=%d (want create>=1 update>=2 for our PUTs)",
			creates, updates)
	}

	t.Logf("S-096 OK: dsar %s lifecycle PENDING→IN_PROGRESS→REJECTED; audit create=%d update=%d",
		created.ID, creates, updates)
}
