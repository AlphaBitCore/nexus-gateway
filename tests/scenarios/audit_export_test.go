// Audit family extension (S-103 — audit log export from catalog §5.13).
// GET /api/admin/admin-audit-logs/export is the compliance-handoff
// surface: an admin exports the audit log for legal/SOC2 review. The
// scenario validates the endpoint's PM-grade invariants — every export
// itself produces a fresh AdminAuditLog "export" row (a meta-audit
// trail), capped record count, and a stable JSON envelope.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS103_AdminAuditExport — PM-grade e2e.
//
// BRAINSTORM (pre): the export endpoint is the bridge between
// runtime audit data and compliance review. Three invariants matter:
//
//   1. The export itself audits — calling export must write a new
//      AdminAuditLog row with action=export, otherwise an admin can
//      silently exfiltrate the entire trail without leaving a
//      breadcrumb. This is the keystone trust property: "who looked
//      at the logs?" must always be answerable.
//   2. The 10k record cap surfaces as `truncated: true` so the UI can
//      warn the operator. A missing truncated flag = silently
//      partial export = compliance disaster.
//   3. The envelope shape is stable: `exportedAt` RFC3339, `entries`
//      array, `truncated` bool. Schema drift here breaks every
//      external SIEM ingestion script.
//
// Cross-service: CP-only DB read + meta-audit write. PM-grade because
// the alternative (200-status smoke) catches none of the above; the
// "export silently doesn't audit" failure mode is the canonical
// "doing security wrong while looking right" pattern.
//
// Assertions:
//   1. GET export returns 200 with {exportedAt, truncated, entries}
//      where exportedAt parses as RFC3339 and truncated is a bool.
//   2. Within 10 s of the call, a new AdminAuditLog row exists with
//      action=export AND entityType ILIKE %audit% (the meta-audit row).
//   3. The number of returned entries is <= 10000 (the documented cap).
func TestS103_AdminAuditExport(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// Mark the moment so we can find the meta-audit row by timestamp.
	startTime := time.Now().UTC()

	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/admin-audit-logs/export?limit=100", nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("export: status %d body=%q", status, truncate(body, 200))
	}
	var resp struct {
		ExportedAt string           `json:"exportedAt"`
		Truncated  bool             `json:"truncated"`
		Entries    []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode export: %v body=%q", err, truncate(body, 300))
	}
	if resp.ExportedAt == "" {
		t.Errorf("envelope missing exportedAt")
	} else if _, err := time.Parse(time.RFC3339, resp.ExportedAt); err != nil {
		t.Errorf("exportedAt not RFC3339: %q (err=%v)", resp.ExportedAt, err)
	}
	if len(resp.Entries) > 10000 {
		t.Errorf("entries=%d exceeds documented cap=10000", len(resp.Entries))
	}
	// truncated should be true iff cap was reached.
	if len(resp.Entries) >= 10000 && !resp.Truncated {
		t.Errorf("truncated=false but entries=%d hit cap — UI would not warn operator",
			len(resp.Entries))
	}

	// Meta-audit row: the export must record itself.
	deadline := time.Now().Add(10 * time.Second)
	var metaAuditID string
	for time.Now().Before(deadline) {
		_ = sc.DB.QueryRow(ctx, `
			SELECT id FROM "AdminAuditLog"
			WHERE "timestamp" >= $1
			  AND action = 'export'
			  AND ("entityType" ILIKE '%audit%' OR "entityType" ILIKE '%log%')
			ORDER BY "timestamp" DESC LIMIT 1
		`, startTime).Scan(&metaAuditID)
		if metaAuditID != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if metaAuditID == "" {
		t.Errorf("META-AUDIT VIOLATION: export action did not write an AdminAuditLog 'export' row within 10 s — admins can exfiltrate without breadcrumb")
	}

	t.Logf("S-103 OK: exportedAt=%s entries=%d truncated=%v metaAudit=%s",
		resp.ExportedAt, len(resp.Entries), resp.Truncated, metaAuditID)
}
