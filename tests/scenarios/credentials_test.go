// Credentials family (S-050..S-053) — verifies provider credential
// admin operations: probe, circuit-reset, rotation.
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

// TestS050_CredentialProbe — PM-grade e2e.
//
// BRAINSTORM (pre): the admin probe endpoint POST
// /api/admin/credentials/:id/probe decrypts the credential, calls
// adapter.Probe against the upstream provider, and returns a
// structured {ok, latencyMs, providerName, …} envelope. It also
// emits an AdminAuditLog "probe" entry capturing only the outcome
// (never the raw key — defense-in-depth against accidental key
// exfiltration).
//
// Cross-service: CP admin handler → AI Gateway
// /internal/v1/credentials/:id/probe → upstream provider → result
// forwarded back. AdminAuditLog row on the CP side.
//
// We probe a seeded moonshot credential because that path is
// known-working (S-001 chats already succeed against it). The
// scenario asserts the envelope structure and the audit row — not
// the specific ok=true outcome, since upstream rate limits or
// credential drift can produce ok=false legitimately.
func TestS050_CredentialProbe(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// Resolve a moonshot credential ID via the admin list — credentials
	// seeded for moonshot were verified earlier in the session.
	listStatus, listBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/credentials?limit=50", nil)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if listStatus != 200 {
		t.Fatalf("list credentials: status %d body=%q", listStatus, truncate(listBody, 200))
	}
	var listResp struct {
		Data []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			ProviderID string `json:"providerId"`
			Enabled    bool   `json:"enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listBody, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var credID string
	for _, c := range listResp.Data {
		if c.Enabled && (c.Name == "moonshot-prod" || c.Name == "moonshot") {
			credID = c.ID
			break
		}
	}
	if credID == "" {
		t.Fatalf("no enabled moonshot credential in admin list (got %d entries)", len(listResp.Data))
	}

	// Probe.
	probeBody, _ := json.Marshal(map[string]any{"timeoutSeconds": 5})
	status, respBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/credentials/"+credID+"/probe", probeBody)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if status != 200 && status != 502 {
		t.Fatalf("probe: status %d body=%q", status, truncate(respBody, 200))
	}
	var probe map[string]any
	if err := json.Unmarshal(respBody, &probe); err != nil {
		t.Fatalf("probe body not JSON: %v body=%q", err, truncate(respBody, 200))
	}
	if _, has := probe["ok"]; !has {
		t.Errorf("probe response missing 'ok' field: %s", truncate(respBody, 200))
	}

	// AdminAuditLog row — probe verb, entity_id = credential id.
	deadline := time.Now().Add(15 * time.Second)
	var auditID string
	for time.Now().Before(deadline) {
		var n int
		err := sc.DB.QueryRow(ctx, `
			SELECT id FROM "AdminAuditLog"
			WHERE "timestamp" > NOW() - INTERVAL '30 seconds'
			  AND "entityId" = $1
			  AND (action = 'probe' OR action ILIKE '%probe%')
			ORDER BY "timestamp" DESC LIMIT 1
		`, credID).Scan(&auditID)
		_ = n
		if err == nil && auditID != "" {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if auditID == "" {
		t.Errorf("no AdminAuditLog probe row for credential %s within 15 s", credID)
	}

	t.Logf("S-050 OK: probe (status=%d ok=%v) + audit=%s body=%s",
		status, probe["ok"], auditID, truncate(respBody, 160))
}

// ensure intg import used (kept here for parity with other tests; the
// scenario doesn't need a direct HTTP client right now).
var _ = fmt.Sprintf
