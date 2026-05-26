// Cross-cutting settings scenarios (S-130 — SIEM test). Verifies the
// SIEM admin config + test endpoint produces a structured envelope
// even when the configured SIEM endpoint is unreachable.
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

// TestS130_SIEMTestEndpoint — PM-grade e2e.
//
// BRAINSTORM (pre): the SIEM integration test admin button
// (POST /api/admin/settings/siem/test) POSTs a synthetic event to
// the configured SIEM URL and returns {ok, error, statusCode, …}.
// The endpoint normalises every outcome (reachable → 200 with
// ok=true; unreachable → 200 with ok=false + error) into a stable
// envelope so the admin UI does not have to special-case transport
// failures. We point the SIEM config at a deliberately-unreachable
// localhost:1 URL — proves the envelope handles transport failures
// without crashing the route.
//
// Cross-service: CP only (no Hub, no AI Gw — SIEM is CP-side).
// system_metadata row "siem.config" persists the config.
//
// Assertions:
//   1. PUT settings/siem persists the config.
//   2. POST settings/siem/test returns 200 with {ok:false, error:...}
//      (envelope contract: never crashes regardless of upstream).
//   3. Cleanup restores the original config so parallel sessions
//      don't see a flipped switch.
func TestS130_SIEMTestEndpoint(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// Snapshot original SIEM config.
	getStatus, getBody, _ := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/settings/siem", nil)
	var original map[string]any
	if getStatus == 200 {
		_ = json.Unmarshal(getBody, &original)
	}
	sc.Cleanup.Register("restore siem config", func() error {
		if len(original) == 0 {
			return nil
		}
		body, _ := json.Marshal(original)
		_, _, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodPut, "/api/admin/settings/siem", body)
		return err
	})

	// Configure SIEM with unreachable URL.
	cfg, _ := json.Marshal(map[string]any{
		"enabled":    true,
		"url":        fmt.Sprintf("http://localhost:1/siem-never-reachable?n=%d", time.Now().UnixNano()),
		"format":     "json",
		"headers":    map[string]string{},
		"eventTypes": []string{"audit"},
	})
	putStatus, putBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPut, "/api/admin/settings/siem", cfg)
	if err != nil {
		t.Fatalf("PUT siem: %v", err)
	}
	if putStatus != 200 {
		t.Fatalf("PUT siem: status %d body=%q", putStatus, truncate(putBody, 200))
	}

	// Trigger the test event.
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/settings/siem/test", nil)
	if err != nil {
		t.Fatalf("test siem: %v", err)
	}
	if status != 200 {
		t.Fatalf("test siem: status %d body=%q", status, truncate(body, 200))
	}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("test siem body not JSON: %v body=%q", err, truncate(body, 200))
	}
	okVal, _ := probe["ok"].(bool)
	if okVal {
		t.Errorf("expected ok=false against unreachable SIEM URL, got ok=true (body=%q)", truncate(body, 200))
	}
	if _, hasErr := probe["error"]; !hasErr {
		t.Errorf("envelope missing 'error' field on failure path: %s", truncate(body, 200))
	}

	t.Logf("S-130 OK: SIEM test envelope structured (ok=%v error=%v)",
		probe["ok"], truncate([]byte(fmt.Sprintf("%v", probe["error"])), 120))
}
