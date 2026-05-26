// Jobs family (S-142 — cross-cutting gap from catalog §10) — verifies
// Hub-managed scheduled jobs can be triggered on demand via admin API.
package scenarios_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS142_TriggerJobRun — PM-grade e2e.
//
// BRAINSTORM (pre): the Hub scheduler runs ~36 jobs on cron-style
// intervals; admins can force a run via POST /api/admin/jobs/:id/trigger.
// We pick "audit-chain-verify" because it's lightweight (validates
// audit-log integrity hashes; reads only, no upstream calls). The
// admin handler proxies to Hub /api/hub/jobs/:id/trigger and writes
// an AdminAuditLog row on 2xx capturing the actor.
//
// Cross-service: CP admin → Hub /api/hub/jobs/:id/trigger → Hub
// scheduler.RunNow() → job_run row in DB → CP AdminAuditLog row
// (action=update, entityType=node since the handler uses
// ResourceNode for hub-proxy verbs).
//
// Assertions:
//   1. Trigger POST returns 2xx with a structured body.
//   2. A fresh job_run row appears for jobId=audit-chain-verify
//      within 15 s.
//   3. AdminAuditLog records the trigger.
func TestS142_TriggerJobRun(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	const jobID = "audit-chain-verify"

	// Snapshot the latest job_run id before the trigger so we can
	// detect a NEW row (audit-chain-verify also runs on its own
	// schedule).
	var lastRunIDBefore string
	_ = sc.DB.QueryRow(ctx,
		`SELECT id FROM job_run WHERE "jobId" = $1 ORDER BY "startedAt" DESC LIMIT 1`,
		jobID).Scan(&lastRunIDBefore)

	// Trigger.
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/jobs/"+jobID+"/trigger", nil)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if status < 200 || status >= 300 {
		t.Fatalf("trigger: status %d body=%q", status, truncate(body, 200))
	}
	if len(body) == 0 {
		t.Errorf("trigger: empty body (status=%d)", status)
	}

	// New job_run row must appear within 15 s. The triggered run may
	// share an id with the snapshot (if scheduler debounces back-to-back
	// triggers) — accept either a different id OR a row with
	// started_at > snapshot moment as evidence the trigger fired.
	deadline := time.Now().Add(15 * time.Second)
	var newRunID string
	var startedAt time.Time
	for time.Now().Before(deadline) {
		err := sc.DB.QueryRow(ctx,
			`SELECT id, "startedAt" FROM job_run WHERE "jobId" = $1 ORDER BY "startedAt" DESC LIMIT 1`,
			jobID).Scan(&newRunID, &startedAt)
		if err == nil && newRunID != "" && newRunID != lastRunIDBefore {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if newRunID == "" || newRunID == lastRunIDBefore {
		t.Fatalf("no new job_run row for %s within 15 s (last id before/after = %q)",
			jobID, lastRunIDBefore)
	}

	// AdminAuditLog for trigger (entityId = jobID, action = update via
	// ResourceNode.Update on hub-proxy verbs).
	auditDeadline := time.Now().Add(15 * time.Second)
	var auditID string
	for time.Now().Before(auditDeadline) {
		_ = sc.DB.QueryRow(ctx, `
			SELECT id FROM "AdminAuditLog"
			WHERE "timestamp" > NOW() - INTERVAL '30 seconds'
			  AND "entityId" = $1
			  AND action = 'update'
			ORDER BY "timestamp" DESC LIMIT 1
		`, jobID).Scan(&auditID)
		if auditID != "" {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if auditID == "" {
		t.Errorf("no AdminAuditLog trigger row for jobId=%s within 15 s", jobID)
	}

	t.Logf("S-142 OK: jobId=%s triggered → run=%s startedAt=%s audit=%s body=%s",
		jobID, newRunID, startedAt.Format(time.RFC3339), auditID, truncate(body, 160))
}
