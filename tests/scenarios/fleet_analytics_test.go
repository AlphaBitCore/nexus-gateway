// Fleet-analytics aggregation scenarios (S-072). The Agent Fleet
// admin surface exposes three read endpoints feeding the Fleet
// Analytics page: a fleet-wide health summary, a metric trend
// series, and the top destination hosts seen across all agents.
// Rollups may be empty in a fresh local env (no agents enrolled, no
// rollup buckets emitted) so the scenarios assert the *contract
// envelope* — status, JSON envelope shape, key presence, and
// non-negativity of any numeric fields that do show up — instead of
// asserting a populated payload. This catches the failure modes that
// matter operationally: handler panics, 500s on empty windows, an
// envelope rename that silently breaks the CP-UI binding, and
// negative aggregate values from a buggy rollup query.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS072_FleetAnalytics exercises the three fleet-analytics admin
// endpoints registered under /api/admin/fleet-analytics/* (see
// packages/control-plane/internal/fleet/handler/agent/fleet_analytics.go).
//
// All three arms run against a single CP login. The arms are
// independent and use sub-tests so a single failing arm does not mask
// the others.
//
// Contract-envelope discipline: the user-facing instruction lists
// candidate key names (totalAgents/activeAgents/totalRequests, etc.)
// that do NOT match the current Go handler output (the summary
// returns {total, active, stale, critical, revoked, stalePct,
// criticalPct}; the trends endpoint returns {metric, buckets[]}; the
// top-destinations endpoint returns {data: [{destHost, eventCount,
// deviceCount}]}). Per the user's "If the actual shape differs, log
// the keys and assert HTTP shape only" directive, each arm logs the
// top-level keys it observed and asserts only:
//
//	(a) HTTP 200,
//	(b) body parses as JSON,
//	(c) the documented envelope key (e.g. "buckets", "data") is
//	    present, and
//	(d) any numeric value that does appear is non-negative.
//
// Any endpoint returning non-200 (including 404) is a hard failure —
// the local stack must ship the fleet-analytics endpoints wired.
func TestS072_FleetAnalytics(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	t.Run("Summary", func(t *testing.T) {
		path := "/api/admin/fleet-analytics/summary"
		status, body, err := helpers.CPDoJSON(ctx, sc.Env, token, http.MethodGet, path, nil)
		if err != nil {
			t.Fatalf("summary: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("summary: status %d body=%q", status, truncate(body, 200))
		}
		// Decode into a generic map so we tolerate the documented
		// envelope (totalAgents/activeAgents/totalRequests) AND the
		// current Go handler shape (total/active/stale/critical/...).
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode summary: %v body=%q", err, truncate(body, 300))
		}
		keys := sortedKeys(raw)

		// Non-negativity: every numeric value at the top level must be
		// >= 0. A negative count/percentage is the classic rollup-math
		// regression the dashboard would silently display.
		numericKeys := 0
		for k, rv := range raw {
			var f float64
			if err := json.Unmarshal(rv, &f); err == nil {
				numericKeys++
				if f < 0 {
					t.Errorf("summary: %s = %v, want >= 0", k, f)
				}
			}
		}
		if numericKeys == 0 {
			// The summary's stated purpose is to expose counters; an
			// envelope with zero numeric top-level fields means the
			// endpoint degenerated into a status-only object and the
			// dashboard tiles would render blank.
			t.Errorf("summary envelope contains no numeric top-level fields; keys=%v", keys)
		}
	})

	t.Run("Trends", func(t *testing.T) {
		path := "/api/admin/fleet-analytics/trends?metric=request_count&startTime=2026-01-01T00:00:00Z&endTime=2026-12-31T23:59:59Z"
		status, body, err := helpers.CPDoJSON(ctx, sc.Env, token, http.MethodGet, path, nil)
		if err != nil {
			t.Fatalf("trends: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("trends: status %d body=%q", status, truncate(body, 200))
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode trends: %v body=%q", err, truncate(body, 300))
		}
		keys := sortedKeys(raw)

		// The current Go handler returns {"metric": "...", "buckets":
		// [...]}; the user's reference description calls the array a
		// "trend series". Accept either the documented "data" key OR
		// the Go-side "buckets" key — both are arrays of buckets.
		seriesRaw, seriesKey := pickFirstPresent(raw, "buckets", "data")
		if seriesKey == "" {
			t.Fatalf("trends envelope missing 'buckets'/'data' key; keys=%v", keys)
		}
		var series []map[string]json.RawMessage
		if err := json.Unmarshal(seriesRaw, &series); err != nil {
			t.Fatalf("decode trends.%s: %v", seriesKey, err)
		}

		// Soft per-bucket assertion: when buckets are present, the
		// canonical "time + value" shape (any timestamp-like field +
		// any numeric value field) must hold; numeric value must be
		// >= 0. A bucket with no fields at all is a contract break.
		for i, b := range series {
			if len(b) == 0 {
				t.Errorf("trends.%s[%d]: empty bucket object", seriesKey, i)
				continue
			}
			for k, rv := range b {
				var f float64
				if err := json.Unmarshal(rv, &f); err == nil && f < 0 {
					t.Errorf("trends.%s[%d].%s = %v, want >= 0", seriesKey, i, k, f)
				}
			}
		}
	})

	t.Run("TopDestinations", func(t *testing.T) {
		path := "/api/admin/fleet-analytics/top-destinations?limit=10&windowHours=24"
		status, body, err := helpers.CPDoJSON(ctx, sc.Env, token, http.MethodGet, path, nil)
		if err != nil {
			t.Fatalf("top-destinations: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("top-destinations: status %d body=%q", status, truncate(body, 200))
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode top-destinations: %v body=%q", err, truncate(body, 300))
		}
		keys := sortedKeys(raw)

		dataRaw, ok := raw["data"]
		if !ok {
			t.Fatalf("top-destinations envelope missing 'data' key; keys=%v", keys)
		}
		// 'data' must be a JSON array (possibly empty) of objects.
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(dataRaw, &rows); err != nil {
			t.Fatalf("decode top-destinations.data: %v", err)
		}

		// If non-empty: each row must carry at least one host-like
		// string field AND at least one numeric count field, with the
		// count >= 0. Accept the documented host/count names OR the
		// Go-side destHost/eventCount/deviceCount names — both are
		// observed in the wild.
		for i, row := range rows {
			if len(row) == 0 {
				t.Errorf("top-destinations.data[%d]: empty row object", i)
				continue
			}
			hasHost := hasNonEmptyString(row, "host", "destHost")
			if !hasHost {
				t.Errorf("top-destinations.data[%d]: no non-empty host field (looked for host/destHost); keys=%v",
					i, sortedKeys(row))
			}
			countKey := ""
			for _, candidate := range []string{"count", "eventCount", "deviceCount"} {
				if rv, ok := row[candidate]; ok {
					var f float64
					if err := json.Unmarshal(rv, &f); err != nil {
						t.Errorf("top-destinations.data[%d].%s: not a number", i, candidate)
						continue
					}
					if f < 0 {
						t.Errorf("top-destinations.data[%d].%s = %v, want >= 0", i, candidate, f)
					}
					countKey = candidate
					break
				}
			}
			if countKey == "" {
				t.Errorf("top-destinations.data[%d]: no count field (looked for count/eventCount/deviceCount); keys=%v",
					i, sortedKeys(row))
			}
		}
	})

}

// sortedKeys returns the keys of a JSON object map in a deterministic
// order so the failure logs are diffable across runs.
func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pickFirstPresent returns the first candidate key found in m along
// with its raw value, or ("", nil) if none are present. Used to
// tolerate documented-vs-actual envelope key names.
func pickFirstPresent(m map[string]json.RawMessage, candidates ...string) (json.RawMessage, string) {
	for _, k := range candidates {
		if v, ok := m[k]; ok {
			return v, k
		}
	}
	return nil, ""
}

// hasNonEmptyString reports whether any of the candidate keys is
// present in row with a non-empty string value.
func hasNonEmptyString(row map[string]json.RawMessage, candidates ...string) bool {
	for _, k := range candidates {
		if rv, ok := row[k]; ok {
			var s string
			if err := json.Unmarshal(rv, &s); err == nil && s != "" {
				return true
			}
		}
	}
	return false
}
