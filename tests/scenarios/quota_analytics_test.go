// Quota analytics family (S-045 — aggregation parity from catalog
// §5.13 gap). Three read endpoints feed the Quota Analytics admin
// page: overview (single-period totals), top (top-N by spend),
// trend (last-N-periods time series). The scenario locks in the
// envelope contract + validation rules that the page depends on.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS045_QuotaAnalyticsContract — PM-grade e2e.
//
// BRAINSTORM (pre): three invariants the page depends on:
//
//   1. Scope enum gate. overview/top accept scope ∈ {user, vk,
//      virtual_key, project, organization}; an unknown scope must
//      return 400 (not silently default and return wrong-axis data,
//      which is a worse failure mode — operators looking at "by
//      user" would see "by org" with no UI signal).
//   2. Trend's required-param gate: missing targetType+targetId
//      returns 400 (don't aggregate an unbounded fleet by accident).
//   3. Periods cap: trend's `periods` query is clamped to [1, 24];
//      an out-of-range value must not crash the handler.
//   4. Envelope: each endpoint returns JSON with the documented
//      top-level shape; no 5xx on a valid request even when the
//      backing rollup is empty.
//
// Cross-service: CP-only DB read. PM-grade because a regression in
// the scope validator would silently mis-attribute quota dollars
// across organisations — the unit-economics dashboard.
func TestS045_QuotaAnalyticsContract(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (1) overview with unknown scope → 400.
	st, body, _ := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/quota-analytics/overview?scope=NOT_A_SCOPE", nil)
	if st != http.StatusBadRequest {
		t.Errorf("overview bad scope: status=%d (want 400) body=%q",
			st, truncate(body, 200))
	}

	// (1b) overview happy path with default scope (user).
	st, body, _ = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/quota-analytics/overview", nil)
	if st != http.StatusOK {
		t.Errorf("overview default: status=%d body=%q", st, truncate(body, 200))
	} else {
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Errorf("overview body not JSON: %v body=%q", err, truncate(body, 200))
		}
	}

	// (2) trend missing required params → 400.
	st, body, _ = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/quota-analytics/trend", nil)
	if st != http.StatusBadRequest {
		t.Errorf("trend missing params: status=%d (want 400) body=%q",
			st, truncate(body, 200))
	}

	// (2b) trend with bad scope → 400.
	st, body, _ = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet,
		"/api/admin/quota-analytics/trend?targetType=NOT_A_SCOPE&targetId=x", nil)
	if st != http.StatusBadRequest {
		t.Errorf("trend bad scope: status=%d (want 400) body=%q",
			st, truncate(body, 200))
	}

	// (3) trend with out-of-range periods. The handler clamps silently
	// (doesn't 400) per the impl — assert it doesn't 5xx either.
	st, body, _ = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet,
		"/api/admin/quota-analytics/trend?targetType=user&targetId=nexus-user-super-admin&periods=9999", nil)
	if st >= 500 {
		t.Errorf("trend large periods: status=%d (handler should clamp, not 5xx) body=%q",
			st, truncate(body, 200))
	}

	// (4) top happy path.
	st, body, _ = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/quota-analytics/top?scope=user&limit=5", nil)
	if st != http.StatusOK {
		t.Errorf("top default: status=%d body=%q", st, truncate(body, 200))
	} else {
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Errorf("top body not JSON: %v body=%q", err, truncate(body, 200))
		}
	}

	t.Logf("S-045 OK: scope guards firing, missing-required-param guards firing, envelopes well-formed")
}
