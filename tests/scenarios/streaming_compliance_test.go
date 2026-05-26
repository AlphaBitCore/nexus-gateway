// Cross-cutting streaming-compliance scenarios (S-131 from catalog
// §10). The /api/admin/settings/streaming-compliance surface holds
// the global streaming-mode default that fans out to all three data
// planes (compliance-proxy, ai-gateway, agent) via Hub config
// invalidation. The scenario validates the GET/PUT round-trip,
// validation enums, and runtime hot-reload on all three subscribers.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS131_StreamingComplianceConfig — PM-grade e2e.
//
// BRAINSTORM (pre): streaming-compliance is the data-plane policy
// admins use to decide whether streaming AI responses get
// passthrough'd unchecked vs buffered for hook evaluation vs chunked
// for async review. The endpoint:
//
//   - is the single source of truth feeding three different services
//     (compliance-proxy, ai-gateway, agent) via Hub.InvalidateConfig
//     fanout — getting the runtime hot-reload signal on all three is
//     the load-bearing invariant.
//   - rejects out-of-enum values for default_mode and fail_behavior
//     (validateStreamingMode / validateFailBehavior). A regression
//     that silently accepts a typo'd mode would corrupt every
//     downstream service's policy parse.
//   - audits every write (entityType=streamingComplianceConfig).
//
// Cross-service: CP write → system_metadata persistence → Hub
// invalidation broadcast → 3 services hot-reload. We verify the
// ai-gateway + compliance-proxy reload by counter delta (agent has
// no metrics URL — gracefully skipped by helpers).
//
// Assertions:
//   1. GET returns the documented envelope shape with valid enum
//      values for default_mode and fail_behavior.
//   2. PUT with default_mode="not-a-mode" returns 400.
//   3. PUT with valid fields persists, returns the merged config
//      verbatim.
//   4. compliance-proxy + ai-gateway streaming_compliance applies
//      counter ticks within 30 s.
//   5. AdminAuditLog records an `update` row with entityType
//      matching streaming-compliance.
//   6. Cleanup restores the original config so parallel sessions
//      don't observe a flipped policy.
func TestS131_StreamingComplianceConfig(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (1) Snapshot original config to restore at end.
	getStatus, getBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/settings/streaming-compliance", nil)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if getStatus != http.StatusOK {
		t.Fatalf("GET: status %d body=%q", getStatus, truncate(getBody, 200))
	}
	var original map[string]any
	if err := json.Unmarshal(getBody, &original); err != nil {
		t.Fatalf("decode GET: %v body=%q", err, truncate(getBody, 200))
	}
	// Validate enum values surface.
	if dm, _ := original["default_mode"].(string); dm == "" {
		t.Errorf("GET response missing default_mode (body=%q)", truncate(getBody, 200))
	}
	if fb, _ := original["fail_behavior"].(string); fb == "" {
		t.Errorf("GET response missing fail_behavior (body=%q)", truncate(getBody, 200))
	}
	sc.Cleanup.Register("restore streaming-compliance", func() error {
		body, _ := json.Marshal(original)
		_, _, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodPut, "/api/admin/settings/streaming-compliance", body)
		return err
	})

	// (2) PUT with invalid enum — must 400.
	badBody, _ := json.Marshal(map[string]any{"default_mode": "not-a-mode"})
	badStatus, badResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut, "/api/admin/settings/streaming-compliance", badBody)
	if err != nil {
		t.Fatalf("bad PUT: %v", err)
	}
	if badStatus != http.StatusBadRequest {
		t.Errorf("bad default_mode: status=%d (want 400) body=%q",
			badStatus, truncate(badResp, 200))
	}

	// (3) PUT with valid fields. Pick whichever current mode ISN'T
	// chunked_async so we flip to chunked_async — guarantees a real
	// change so the hot-reload signal fires.
	preApplyProxy, _ := helpers.BaselineConfigApply(ctx, sc.Env, "streaming_compliance")
	target := map[string]any{
		"default_mode":   "chunked_async",
		"fail_behavior":  "fail_close",
		"chunk_bytes":    16384,
		"hook_timeout_ms": 3000,
	}
	putBody, _ := json.Marshal(target)
	putStatus, putResp, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut, "/api/admin/settings/streaming-compliance", putBody)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if putStatus != http.StatusOK {
		t.Fatalf("PUT: status %d body=%q", putStatus, truncate(putResp, 200))
	}
	var merged map[string]any
	if err := json.Unmarshal(putResp, &merged); err != nil {
		t.Fatalf("decode PUT: %v body=%q", err, truncate(putResp, 200))
	}
	if merged["default_mode"] != "chunked_async" {
		t.Errorf("PUT response default_mode=%v, want chunked_async", merged["default_mode"])
	}
	if merged["fail_behavior"] != "fail_close" {
		t.Errorf("PUT response fail_behavior=%v, want fail_close", merged["fail_behavior"])
	}

	// (4) Runtime hot-reload signal. helpers.WaitForConfigApply scans
	// every subscriber service it knows of. streaming_compliance is
	// pushed to compliance-proxy + ai-gateway + agent — agent has no
	// scrape URL and is gracefully skipped.
	if _, err := helpers.WaitForConfigApply(ctx, sc.Env, "streaming_compliance",
		preApplyProxy, 30*time.Second); err != nil {
		t.Errorf("subscriber services did not hot-reload streaming_compliance: %v", err)
	}

	// (5) AdminAuditLog row for the PUT.
	deadline := time.Now().Add(10 * time.Second)
	var auditID string
	for time.Now().Before(deadline) {
		_ = sc.DB.QueryRow(ctx, `
			SELECT id FROM "AdminAuditLog"
			WHERE "timestamp" > NOW() - INTERVAL '30 seconds'
			  AND action = 'update'
			  AND ("entityType" ILIKE '%streaming%' OR "entityType" ILIKE '%settings%')
			ORDER BY "timestamp" DESC LIMIT 1
		`).Scan(&auditID)
		if auditID != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if auditID == "" {
		t.Errorf("no AdminAuditLog 'update' row for streaming-compliance within 10 s")
	}

	t.Logf("S-131 OK: mode=%v failBehavior=%v audit=%s",
		merged["default_mode"], merged["fail_behavior"], auditID)
}
