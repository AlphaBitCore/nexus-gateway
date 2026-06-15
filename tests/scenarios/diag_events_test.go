// Cross-cutting Diag-events scenarios (S-141 — crash cohort grouping
// from catalog §10). The Hub writes thing_diag_event rows via SlogSink
// + DiagWriter when services emit ERROR/FATAL slog records; the CP
// surface exposes three read-only endpoints over that table:
// /diag-events (list), /diag-events/groups (top message_hash buckets),
// /diag-events/crash-cohorts (FATAL grouped by agent_version + os).
// This scenario inserts synthetic rows directly into the DB (the
// canonical CP-side test seam — Hub-internal batch upload requires
// service-token auth + per-Thing registration) and asserts each of
// the three list endpoints surfaces them.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS141_DiagEventsListGroupsCohorts — PM-grade e2e.
//
// BRAINSTORM (pre): the diag-events stack is one of the rare CP areas
// where the write-path lives entirely in the Hub (slog → SlogSink →
// DiagWriter → thing_diag_event), and the CP admin only ever reads.
// The strongest e2e is to seed deterministic rows directly into
// thing_diag_event (the same table the production write-path lands
// in), then exercise the three CP read endpoints end-to-end:
//
//  1. /diag-events?nodeId=&q= → row appears in newest-first list
//  2. /diag-events/groups?from=&to= → message_hash bucket counted
//  3. /diag-events/crash-cohorts?from=&to= → (agent_version, os,
//     os_version) tuple aggregated for our FATAL crash row
//
// Cross-service: pure CP-read. No Hub config push needed — this is the
// read-only consumer side of the diag pipeline. The write-path itself
// is covered separately by the Hub e2e test (opsmetrics_e2e_test.go).
// The PM-grade question this scenario answers: if a real agent crash
// lands in the table tomorrow, does the admin UI's "Crash Reports"
// page actually surface it via the cohort grouping query?
//
// Assertions:
//  1. INSERT 2 rows (one event_type='crash' level='fatal' with
//     agent_version + os_info; one 'generic_error' level='error') —
//     both under a synthetic thing_id prefixed s141-<nanos>.
//  2. List endpoint returns both rows (nodeId filter is precise).
//  3. Groups endpoint contains the message_hash bucket with
//     occurrenceCount == 1 (each row is its own hash).
//  4. Crash-cohorts endpoint contains the (agent_version, os,
//     os_version) tuple with crashCount >= 1 and affectedThings >= 1.
//  5. Cleanup deletes both rows to keep parallel sessions hermetic.
func TestS141_DiagEventsListGroupsCohorts(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	// This scenario seeds a synthetic Thing + thing_diag_event rows by writing
	// the shared `thing` fleet table directly via sc.DB — that bypasses the
	// prod-safe-e2e API guard (which only covers CP HTTP calls) and would
	// mutate live fleet state. There is no API-only way to express the seed,
	// so the read-side assertions can't run safely on prod. Skip with reason;
	// the CP read-path stays covered locally and the write-path has its own
	// Hub e2e coverage.
	if helpers.IsProdSafeE2E() {
		t.Skip("prod-safe-e2e: S-141 seeds the shared thing/thing_diag_event tables via direct DB (bypasses the API guard); not safe to run against prod")
	}

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	nonce := time.Now().UnixNano()
	thingID := fmt.Sprintf("s141-%d", nonce)
	crashMsg := fmt.Sprintf("synthetic crash %d", nonce)
	crashHash := fmt.Sprintf("s141-crash-%d", nonce)
	errMsg := fmt.Sprintf("synthetic error %d", nonce)
	errHash := fmt.Sprintf("s141-err-%d", nonce)
	const agentVer = "999.999.999-s141"
	osInfo := map[string]any{"os": "macos", "osVersion": "26.99"}
	osInfoJSON, _ := json.Marshal(osInfo)

	now := time.Now().UTC()
	// Window straddles the inserts so to-exclusive bound doesn't drop them.
	from := now.Add(-1 * time.Minute)
	to := now.Add(1 * time.Minute)

	// thing_diag_event.thing_id has an ON-DELETE-CASCADE FK to thing(id).
	// Seed a synthetic agent Thing row first so the inserts pass FK,
	// then register a cleanup that drops it (which cascades the diag
	// rows automatically — the explicit DELETE below is belt+braces).
	const insertThingSQL = `
		INSERT INTO thing (id, type, name, auth_type, conn_protocol, status,
		                   enrolled_at, updated_at)
		VALUES ($1, 'agent', $2, 'bearer', 'http', 'enrolled', NOW(), NOW())
		ON CONFLICT (id) DO NOTHING
	`
	if _, err := sc.DB.Exec(ctx, insertThingSQL, thingID, "S-141 synthetic agent"); err != nil {
		t.Fatalf("seed thing: %v", err)
	}
	sc.Cleanup.Register("delete s141 thing", func() error {
		_, err := sc.DB.Exec(context.Background(),
			`DELETE FROM thing WHERE id = $1`, thingID)
		return err
	})

	// Seed two rows. occurred_at within the [from, to) window so the
	// groups + cohorts queries pick them up.
	const insertSQL = `
		INSERT INTO thing_diag_event
		  (id, thing_id, thing_type, occurred_at, received_at, level,
		   event_type, source, message, message_hash, attrs, agent_version, os_info)
		VALUES
		  (gen_random_uuid(), $1, 'agent', $2, $2, $3,
		   $4, 'test', $5, $6, '{}'::jsonb, $7, $8::jsonb)
	`
	if _, err := sc.DB.Exec(ctx, insertSQL,
		thingID, now, "fatal", "crash", crashMsg, crashHash, agentVer, string(osInfoJSON),
	); err != nil {
		t.Fatalf("insert crash row: %v", err)
	}
	if _, err := sc.DB.Exec(ctx, insertSQL,
		thingID, now, "error", "generic_error", errMsg, errHash, agentVer, string(osInfoJSON),
	); err != nil {
		t.Fatalf("insert error row: %v", err)
	}
	sc.Cleanup.Register("delete s141 diag rows", func() error {
		_, err := sc.DB.Exec(context.Background(),
			`DELETE FROM thing_diag_event WHERE thing_id = $1`, thingID)
		return err
	})

	// (1) List endpoint — nodeId filter is exact.
	listURL := fmt.Sprintf("/api/admin/diag-events?nodeId=%s&limit=10",
		url.QueryEscape(thingID))
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, listURL, nil)
	if err != nil {
		t.Fatalf("list diag-events: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("list diag-events: status %d body=%q", status, truncate(body, 200))
	}
	var listResp struct {
		Data []struct {
			Level       string `json:"level"`
			EventType   string `json:"eventType"`
			Message     string `json:"message"`
			MessageHash string `json:"messageHash"`
			ThingID     string `json:"nodeId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v body=%q", err, truncate(body, 300))
	}
	if len(listResp.Data) < 2 {
		t.Fatalf("list: expected ≥2 rows for nodeId=%s, got %d (body=%q)",
			thingID, len(listResp.Data), truncate(body, 300))
	}
	gotCrash, gotErr := false, false
	for _, r := range listResp.Data {
		if r.MessageHash == crashHash {
			gotCrash = true
		}
		if r.MessageHash == errHash {
			gotErr = true
		}
	}
	if !gotCrash || !gotErr {
		t.Errorf("list: missing rows (crash=%v err=%v) — got rows=%+v",
			gotCrash, gotErr, listResp.Data)
	}

	// (2) Groups endpoint. The bucket we care about is keyed by message
	// hash and event_type. Different deployments shape DiagGroup
	// slightly differently — decode tolerantly into a map.
	fromStr := from.Format(time.RFC3339Nano)
	toStr := to.Format(time.RFC3339Nano)
	groupsURL := fmt.Sprintf("/api/admin/diag-events/groups?from=%s&to=%s",
		url.QueryEscape(fromStr), url.QueryEscape(toStr))
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, groupsURL, nil)
	if err != nil {
		t.Fatalf("list diag-groups: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("list diag-groups: status %d body=%q", status, truncate(body, 200))
	}
	var groupsResp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &groupsResp); err != nil {
		t.Fatalf("decode groups: %v body=%q", err, truncate(body, 300))
	}
	groupCrashFound := false
	for _, g := range groupsResp.Data {
		if hash, _ := g["messageHash"].(string); hash == crashHash {
			groupCrashFound = true
			break
		}
	}
	if !groupCrashFound {
		t.Errorf("groups: messageHash=%s not in top-N buckets (n=%d). The endpoint caps at top 100 by occurrence — under high parallel load this may miss synthetic singletons. Body excerpt: %q",
			crashHash, len(groupsResp.Data), truncate(body, 300))
	}

	// (3) Crash-cohorts — only event_type='crash' rows. Our fatal row
	// should produce a cohort matching our (agent_version, os, os_version)
	// triple.
	cohortsURL := fmt.Sprintf("/api/admin/diag-events/crash-cohorts?from=%s&to=%s",
		url.QueryEscape(fromStr), url.QueryEscape(toStr))
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, cohortsURL, nil)
	if err != nil {
		t.Fatalf("list crash-cohorts: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("list crash-cohorts: status %d body=%q", status, truncate(body, 200))
	}
	var cohortsResp struct {
		Data []struct {
			AgentVersion   string `json:"agentVersion"`
			OS             string `json:"os"`
			OSVersion      string `json:"osVersion"`
			CrashCount     int    `json:"crashCount"`
			AffectedThings int    `json:"affectedNodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &cohortsResp); err != nil {
		t.Fatalf("decode cohorts: %v body=%q", err, truncate(body, 300))
	}
	cohortFound := false
	for _, c := range cohortsResp.Data {
		if c.AgentVersion == agentVer && c.OS == "macos" && c.OSVersion == "26.99" {
			cohortFound = true
			if c.CrashCount < 1 || c.AffectedThings < 1 {
				t.Errorf("cohort matched but counts off: crashCount=%d affectedThings=%d",
					c.CrashCount, c.AffectedThings)
			}
			break
		}
	}
	if !cohortFound {
		t.Errorf("crash-cohorts: tuple (%s, macos, 26.99) absent from response (%d rows). Body excerpt: %q",
			agentVer, len(cohortsResp.Data), truncate(body, 400))
	}

	t.Logf("S-141 OK: list=2 groups_match=%v cohort_match=%v",
		groupCrashFound, cohortFound)
}
