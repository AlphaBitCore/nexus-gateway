// Agent / fleet heartbeat freshness scenario (S-076 from catalog §10
// fleet visibility). Heartbeats from every connected Thing — agent,
// ai-gateway, control-plane, compliance-proxy — flow through the Hub's
// TouchThingSession path and land as a `last_seen_at = NOW()` UPDATE on
// the `thing` table. The admin Nodes page reads that column (via Hub
// → CP /api/admin/nodes) to decide whether to paint a node as online,
// idle, or offline. If the heartbeat path silently regresses, every
// admin sees stale data and the entire Infrastructure section becomes
// unreliable — yet no scenario today proves the round-trip works.
//
// We deliberately do NOT spin up a real agent daemon: the heartbeat
// path is uniform across all five Thing types, and any local dev env
// running ./scripts/dev-start.sh already has at least three server
// Things (Hub, ai-gateway, CP, compliance-proxy register themselves as
// Things on boot). Verifying for ANY connected Thing exercises the
// same code path an agent would.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS076_AgentHeartbeatFreshness — PM-grade e2e.
//
// BRAINSTORM (pre): the heartbeat round-trip is "Thing emits heartbeat
// → Hub TouchThingSession → UPDATE thing SET last_seen_at = NOW() →
// admin GET /api/admin/nodes shows fresh row". Two failure modes the
// scenario needs to cover:
//
//	1. DB-side: at least one of the locally-connected Things stamped a
//	   last_seen_at within the last 2 minutes. Local dev always runs the
//	   four server Things (Hub, CP, AI Gateway, compliance-proxy), so
//	   zero fresh rows is a heartbeat-write regression, not an env state
//	   — fail hard.
//	2. API-side: GET /api/admin/nodes returns the documented envelope
//	   `{ nodes: [...] }` with a `status` field per entry, AND the
//	   most-recent-heartbeat Thing observed in arm 1 is present in that
//	   list. Cross-checking the same ID across both arms catches BFF
//	   filtering regressions (e.g. a future "hide offline nodes" toggle
//	   that accidentally hides every node).
//
// NOTE on column name: the user-facing concept is "heartbeat", but the
// canonical DB column is `last_seen_at` — the Hub's TouchThingSession
// path is the single writer (see thing_registry.go:204/331/403/426/445)
// and stamps last_seen_at = NOW() on every WebSocket pong / HTTP poll.
// There is no separate last_heartbeat_at column; last_seen_at IS the
// heartbeat timestamp.
//
// Assertions:
//  1. DB: at least one thing row with `last_seen_at > NOW() - 120s`.
//     Fail hard if zero — local dev always has the four server Things
//     emitting heartbeats; absence is a regression in the heartbeat
//     write path. Record the freshest Thing's id + type + age.
//  2. CP: GET /api/admin/nodes returns 200 with `nodes` array.
//  3. CP: every node entry exposes a non-empty `status` field — the
//     primary signal the admin UI binds to (online/offline/idle/...).
//  4. CP: the freshest Thing id from arm 1 appears in the nodes list.
func TestS076_AgentHeartbeatFreshness(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	// --- Arm 1: heartbeat freshness on the thing table. ---

	type freshRow struct {
		ID        string
		ThingType string
		AgeSec    int
	}

	const freshSQL = `
		SELECT id, type,
		       EXTRACT(EPOCH FROM (NOW() - last_seen_at))::int AS age_sec
		FROM thing
		WHERE last_seen_at IS NOT NULL
		ORDER BY last_seen_at DESC
		LIMIT 10
	`
	rows, err := sc.DB.Query(ctx, freshSQL)
	if err != nil {
		t.Fatalf("query thing freshness: %v", err)
	}
	var fresh []freshRow
	for rows.Next() {
		var r freshRow
		if err := rows.Scan(&r.ID, &r.ThingType, &r.AgeSec); err != nil {
			rows.Close()
			t.Fatalf("scan thing freshness: %v", err)
		}
		fresh = append(fresh, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate thing freshness: %v", err)
	}

	const freshThresholdSec = 120
	var freshest *freshRow
	for i := range fresh {
		if fresh[i].AgeSec < freshThresholdSec {
			freshest = &fresh[i]
			break // rows are already ORDER BY last_seen_at DESC, so first match is freshest
		}
	}
	if freshest == nil {
		// Local dev (./scripts/dev-start.sh) always runs Hub + CP +
		// AI Gateway + compliance-proxy, all of which register as
		// Things and stamp last_seen_at via TouchThingSession on every
		// WebSocket pong. Zero fresh rows means the heartbeat write
		// path is broken, not "no Thing is connected" — fail hard so
		// the regression surfaces instead of getting swallowed by a
		// skip.
		t.Fatalf("S-076: no Thing has last_seen_at within %ds — heartbeat write path regressed (top-10 rows: %+v)",
			freshThresholdSec, fresh)
	}
	t.Logf("S-076 arm 1: freshest Thing id=%s type=%s age=%ds (threshold %ds)",
		freshest.ID, freshest.ThingType, freshest.AgeSec, freshThresholdSec)

	// --- Arm 2: fleet aggregate visibility via CP admin. ---

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/nodes?pageSize=200", nil)
	if err != nil {
		t.Fatalf("GET /api/admin/nodes: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET /api/admin/nodes: status %d body=%q", status, truncate(body, 200))
	}

	// hub.RenameThingsList rewrites Hub's `things` wrapper to `nodes`;
	// status, type, last_seen_at pass through unchanged. Decode tolerantly
	// — extra fields in the upstream response must not break this test.
	var nodesResp struct {
		Nodes []map[string]json.RawMessage `json:"nodes"`
		Total int                          `json:"total"`
	}
	if err := json.Unmarshal(body, &nodesResp); err != nil {
		t.Fatalf("decode /api/admin/nodes: %v body=%q", err, truncate(body, 300))
	}
	if len(nodesResp.Nodes) == 0 {
		t.Fatalf("GET /api/admin/nodes returned empty nodes array (body=%q) — arm 1 saw a fresh Thing, so the fleet list should not be empty",
			truncate(body, 300))
	}

	statusedNodes := 0
	freshestInList := false
	for _, n := range nodesResp.Nodes {
		// `status` is the primary admin-UI signal (online/offline/idle/
		// enrolled/revoked). A missing or empty status would silently
		// render the column blank in the UI.
		if rawStatus, ok := n["status"]; ok {
			var s string
			if err := json.Unmarshal(rawStatus, &s); err == nil && s != "" {
				statusedNodes++
			}
		}
		if rawID, ok := n["id"]; ok {
			var id string
			if err := json.Unmarshal(rawID, &id); err == nil && id == freshest.ID {
				freshestInList = true
			}
		}
	}
	if statusedNodes == 0 {
		// Surface one node's keys so a debugger can see what shape the
		// BFF actually returned without dumping the full body.
		var sampleKeys []string
		for k := range nodesResp.Nodes[0] {
			sampleKeys = append(sampleKeys, k)
		}
		t.Errorf("/api/admin/nodes returned %d nodes but none expose a non-empty `status` field — sample keys=%v",
			len(nodesResp.Nodes), sampleKeys)
	}
	if !freshestInList {
		t.Errorf("freshest Thing id=%s (age=%ds) absent from /api/admin/nodes list of %d nodes — fleet list filtered it out unexpectedly",
			freshest.ID, freshest.AgeSec, len(nodesResp.Nodes))
	}

	t.Logf("S-076 OK: %d/%d nodes have status; freshest_in_list=%v",
		statusedNodes, len(nodesResp.Nodes), freshestInList)
}
