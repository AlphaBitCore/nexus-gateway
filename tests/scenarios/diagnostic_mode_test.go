// Cross-cutting Diagnostic-Mode scenario (S-073 — agent diagnostic-mode
// lifecycle). The diag-mode subsystem opens a time-bounded window on an
// agent Thing during which verbose telemetry is collected; the admin
// surface exposes four endpoints under /api/admin/agents/*:
//
//	GET    /api/admin/agents/diagnostic-mode          → list active windows
//	POST   /api/admin/agents/:nodeId/diagnostic-mode  → enable for one
//	DELETE /api/admin/agents/:nodeId/diagnostic-mode  → disable
//	POST   /api/admin/agents/diagnostic-mode/bulk     → enable for many
//
// Server-side: enable writes thing_diag_mode_window + sets
// thing.metadata.diagModeUntil in a single tx (handler:
// packages/control-plane/internal/infrastructure/infra/diagmode.go;
// store: packages/control-plane/internal/observability/opsmetrics/opsstore).
// Hub config-invalidate fires fire-and-forget so connected agents drop
// their cached desired-state on the next heartbeat.
//
// IAM: admin:diagnostic-mode.read + admin:diagnostic-mode.update.
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

// TestS073_DiagnosticModeLifecycle — PM-grade e2e covering the four-endpoint
// diagnostic-mode lifecycle for a single agent Thing.
//
// BRAINSTORM (pre): the diag-mode surface is unusual in that its primary
// state lives on TWO related tables (thing_diag_mode_window for history +
// thing.metadata.diagModeUntil for the agent shadow read-path), and the
// listing endpoint computes "active" purely from ended_at > now() on
// thing_diag_mode_window. The strongest e2e is therefore to drive the
// admin endpoints in their natural lifecycle order (list → enable → list
// → disable → list) and assert each transition is observable through the
// list endpoint — without touching the DB-backed shadow, which has its
// own Hub-side test coverage.
//
// Cross-service: pure CP admin. The Hub config-invalidate side-effect is
// fire-and-forget and not asserted here (covered by hub_runtime_test.go).
// The PM-grade question this scenario answers: if SRE opens diagnostic
// mode on an agent tomorrow via the CP-UI Infrastructure → Nodes drawer,
// does it actually appear in the live "agents in diag mode" listing, and
// does disabling it remove it from that list?
//
// Arms:
//  1. List initially — GET .../diagnostic-mode returns 200 with `data`
//     array (may be empty depending on environment).
//  2. Probe for an existing agent Thing — SELECT id FROM thing WHERE
//     type='agent' LIMIT 1. If none, t.Skipf — fresh local envs do not
//     guarantee an agent registration.
//  3. Enable — POST .../:id/diagnostic-mode with until=now+1h, expect 200.
//  4. Verify in list — agent appears with matching `endedAt` near our
//     `until` (sub-second skew tolerated; round to UTC seconds).
//  5. Disable — DELETE .../:id/diagnostic-mode, expect 200/204. Cleanup
//     registered defensively in case the assertions above panic.
//  6. Final list verification — agent no longer present.
func TestS073_DiagnosticModeLifecycle(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (1) List initially. Endpoint must answer 200 even when zero windows
	// are active — the empty-slice contract is part of the CP-UI's load
	// path and a 500 here would mean the Infrastructure page falls over
	// on first paint.
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/agents/diagnostic-mode", nil)
	if err != nil {
		t.Fatalf("list (initial): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("list (initial): status %d body=%q", status, truncate(body, 200))
	}
	var initialList struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &initialList); err != nil {
		t.Fatalf("decode list (initial): %v body=%q", err, truncate(body, 300))
	}
	if initialList.Data == nil {
		t.Errorf("list (initial): `data` field absent or null — must be [] not null for CP-UI parity (body=%q)",
			truncate(body, 300))
	}

	// (2) Probe for an existing agent Thing. Fresh local environments
	// may not have an agent enrolled (the agent registers via the macOS
	// .pkg install flow, not via dev-start.sh), so skip rather than fail.
	var thingID string
	err = sc.DB.QueryRow(ctx,
		`SELECT id FROM thing WHERE type = 'agent' LIMIT 1`,
	).Scan(&thingID)
	if err != nil {
		// pgx returns ErrNoRows here. The diagnostic-mode surface is
		// structurally untestable without an enrolled agent — fail hard
		// (per the E86 hardening rule: precondition gaps are fixed in
		// setup, not silently skipped). Recovery: seed an agent Thing row
		// (see tools/db-migrate/seed) or run an agent registration.
		t.Fatalf("S-073 precondition unmet: no agent Thing in DB. Seed one via tools/db-migrate (thing row with type='agent') or register a local agent before re-running (err=%v)", err)
	}
	if thingID == "" {
		t.Fatalf("S-073 precondition unmet: no agent Thing in DB. Seed one via tools/db-migrate (thing row with type='agent') or register a local agent before re-running")
	}

	// (3) Enable diag-mode for the probed agent. Until = now + 1h sits
	// well inside the handler's 24h cap (maxDiagModeDuration in
	// diagmode.go). Reason is a free-text label echoed back via the
	// audit event.
	until := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Second)
	reason := fmt.Sprintf("S-073 test %d", time.Now().UnixNano())
	enableBody := mustMarshal(t, map[string]any{
		"until":  until.Format(time.RFC3339),
		"reason": reason,
	})

	// Register disable cleanup BEFORE the enable POST so a panic in any
	// downstream assertion still closes the window on the agent. The
	// idempotent path returns 404 (WINDOW_NOT_FOUND) once we've already
	// disabled it explicitly in step 5 — swallow that case here.
	sc.Cleanup.Register("s073 disable diag-mode (defensive)", func() error {
		_, _, derr := helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete,
			"/api/admin/agents/"+thingID+"/diagnostic-mode", nil)
		return derr
	})

	enablePath := "/api/admin/agents/" + thingID + "/diagnostic-mode"
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, enablePath, enableBody)
	if err != nil {
		t.Fatalf("enable diag-mode: %v", err)
	}
	if status == http.StatusNotFound {
		// Per E86 hardening: race conditions are real failures the test
		// suite must surface. If the agent Thing vanished between SELECT
		// and POST, that's a setup leak — fail hard so the maintainer can
		// investigate the concurrent-cleanup source.
		t.Fatalf("enable diag-mode: agent %s returned 404 after SELECT. Likely concurrent cleanup leak; fix the scenario that deletes shared agent rows before re-running", thingID)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("enable diag-mode: status %d body=%q", status, truncate(body, 300))
	}

	// (4) Verify the just-enabled window is in the active list. The
	// response shape is `{"data": [DiagModeWindow,…]}` where each
	// window carries `nodeId`, `endedAt`, `startedAt`, `reason`, etc.
	// We do not assert the full row — only that our thing is present
	// and its endedAt is within 2s of our requested until (sub-second
	// truncation + clock skew tolerated; anything larger means the
	// store dropped or rewrote the timestamp).
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/agents/diagnostic-mode", nil)
	if err != nil {
		t.Fatalf("list (post-enable): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("list (post-enable): status %d body=%q", status, truncate(body, 200))
	}
	var postEnableList struct {
		Data []struct {
			NodeID  string    `json:"nodeId"`
			EndedAt time.Time `json:"endedAt"`
			Reason  *string   `json:"reason,omitempty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &postEnableList); err != nil {
		t.Fatalf("decode list (post-enable): %v body=%q", err, truncate(body, 300))
	}
	found := false
	for _, w := range postEnableList.Data {
		if w.NodeID != thingID {
			continue
		}
		found = true
		skew := w.EndedAt.Sub(until)
		if skew < -2*time.Second || skew > 2*time.Second {
			t.Errorf("list (post-enable): endedAt skew too large for nodeId=%s — got %s want ~%s (skew=%s)",
				thingID, w.EndedAt.Format(time.RFC3339Nano),
				until.Format(time.RFC3339Nano), skew)
		}
		if w.Reason == nil || *w.Reason != reason {
			gotReason := "<nil>"
			if w.Reason != nil {
				gotReason = *w.Reason
			}
			t.Errorf("list (post-enable): reason mismatch for nodeId=%s — got %q want %q",
				thingID, gotReason, reason)
		}
		break
	}
	if !found {
		t.Fatalf("list (post-enable): nodeId=%s not present in active windows (%d rows, body=%q)",
			thingID, len(postEnableList.Data), truncate(body, 400))
	}

	// (5) Disable. Handler returns 200 with {"ok": true} on success;
	// some proxies normalise to 204 No Content, so accept both.
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodDelete, enablePath, nil)
	if err != nil {
		t.Fatalf("disable diag-mode: %v", err)
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("disable diag-mode: status %d body=%q", status, truncate(body, 300))
	}

	// (6) Final list — our thing must be absent. We re-list rather than
	// trust the disable response so the assertion exercises the same
	// read path the CP-UI uses to refresh after a disable click.
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/agents/diagnostic-mode", nil)
	if err != nil {
		t.Fatalf("list (post-disable): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("list (post-disable): status %d body=%q", status, truncate(body, 200))
	}
	var postDisableList struct {
		Data []struct {
			NodeID string `json:"nodeId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &postDisableList); err != nil {
		t.Fatalf("decode list (post-disable): %v body=%q", err, truncate(body, 300))
	}
	for _, w := range postDisableList.Data {
		if w.NodeID == thingID {
			t.Errorf("list (post-disable): nodeId=%s still present after DELETE (body=%q)",
				thingID, truncate(body, 400))
			break
		}
	}

	t.Logf("S-073 OK: agent=%s enable→list-hit→disable→list-clear lifecycle verified", thingID)
}
