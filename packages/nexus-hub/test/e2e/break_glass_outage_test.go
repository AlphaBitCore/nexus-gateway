//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/testharness"
)

// TestBreakGlassShadow_RecoveryAfterOutage verifies the break-glass spool /
// replay contract from the Hub side. When a data-plane proxy tries to
// deliver a break-glass shadow report during a transient Hub outage, the
// first attempt must fail hard and leave no partial state; the retry that
// lands after the outage clears must succeed and produce exactly one
// emergency_override audit row.
//
// Scenario:
//  1. A fault layer wraps the Hub and returns 503 for all requests.
//  2. A break-glass shadow report is posted — Hub is unreachable, response
//     is 503 (proxy would spool this to its pending-buffer on disk).
//  3. The outage lifts; the same payload is re-posted (ReplayPending in the
//     proxy drains the buffer on reconnect).
//  4. The retry returns 200; thing_config_template is upserted at the
//     reported version; exactly one emergency_override row exists.
//
// Distinct from TestBreakGlassVersionConflict_HTTP, which tests version
// collision on a live Hub — this test isolates the outage + retry contract.
func TestBreakGlassShadow_RecoveryAfterOutage(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	hub := testharness.NewForTest(t)

	// Wrap the Hub handler in a fault-injector so we can simulate a short
	// outage without tearing down Postgres or the httptest server.
	var faulty atomic.Bool
	wrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if faulty.Load() {
			http.Error(w, `{"error":"service unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		hub.Handler().ServeHTTP(w, r)
	})
	ts := httptest.NewServer(wrap)
	t.Cleanup(ts.Close)

	const (
		thingID   = "proxy-bg-outage"
		thingType = "compliance-proxy"
		configKey = "killswitch"
		tokenID   = "cafef00d"
	)

	_ = hub.IssueEnrollmentTokenOfType(t, thingID, thingType)

	t.Cleanup(func() {
		ctx := context.Background()
		pool := hub.Store().Pool()
		_, _ = pool.Exec(ctx,
			`DELETE FROM config_change_event WHERE thing_type=$1 AND config_key=$2`,
			thingType, configKey)
		_, _ = pool.Exec(ctx,
			`DELETE FROM thing_config_template WHERE type=$1 AND config_key=$2`,
			thingType, configKey)
	})

	const reportedVer = int64(1)
	payload, err := json.Marshal(map[string]any{
		"id":           thingID,
		"reported":     map[string]any{configKey: map[string]any{"enabled": true}},
		"reportedVer":  reportedVer,
		"keyVersions":  map[string]int64{configKey: reportedVer},
		"reason":       "break_glass",
		"sourceIp":     "10.0.0.42",
		"actorTokenId": tokenID,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	postURL := ts.URL + "/api/internal/things/shadow"
	token := hub.ServiceToken()

	// --- Outage: first delivery attempt fails with 503. ---
	faulty.Store(true)
	if status := doShadowPost(t, postURL, token, payload); status != http.StatusServiceUnavailable {
		t.Fatalf("during outage: got HTTP %d, want 503", status)
	}

	// No partial state: Hub never saw the request, so the template row and
	// any emergency_override event must be absent.
	if _, ok := queryTemplateVersionDirect(t, hub, thingType, configKey); ok {
		t.Fatal("during outage: template row must not exist (Hub was unreachable)")
	}

	// --- Recovery: outage lifts, client replays. ---
	faulty.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	var replayStatus int
	for time.Now().Before(deadline) {
		replayStatus = doShadowPost(t, postURL, token, payload)
		if replayStatus == http.StatusOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if replayStatus != http.StatusOK {
		t.Fatalf("after outage lifted: got HTTP %d, want 200", replayStatus)
	}

	// Template upserted at the reported version.
	tplVer, ok := queryTemplateVersionDirect(t, hub, thingType, configKey)
	if !ok {
		t.Fatal("after recovery: template row missing")
	}
	if tplVer != reportedVer {
		t.Errorf("template version = %d, want %d", tplVer, reportedVer)
	}

	// Exactly one emergency_override row with the expected forensic fields.
	var (
		count      int
		actorID    string
		action     string
		sourceIP   string
		newVersion int64
	)
	err = hub.Store().Pool().QueryRow(context.Background(),
		`SELECT COUNT(*) FROM config_change_event
		 WHERE thing_type=$1 AND config_key=$2 AND emergency_override=true`,
		thingType, configKey,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count emergency_override rows: %v", err)
	}
	if count != 1 {
		t.Errorf("emergency_override rows = %d, want exactly 1", count)
	}

	err = hub.Store().Pool().QueryRow(context.Background(),
		`SELECT actor_id, action, COALESCE(source_ip, ''), new_version
		 FROM config_change_event
		 WHERE thing_type=$1 AND config_key=$2 AND emergency_override=true
		 ORDER BY timestamp DESC LIMIT 1`,
		thingType, configKey,
	).Scan(&actorID, &action, &sourceIP, &newVersion)
	if err != nil {
		t.Fatalf("select emergency_override row: %v", err)
	}
	if actorID != "break-glass:"+tokenID {
		t.Errorf("actor_id = %q, want break-glass:%s", actorID, tokenID)
	}
	if action != "emergency_override" {
		t.Errorf("action = %q, want emergency_override", action)
	}
	if sourceIP != "10.0.0.42" {
		t.Errorf("source_ip = %q, want 10.0.0.42", sourceIP)
	}
	if newVersion != reportedVer {
		t.Errorf("new_version = %d, want %d", newVersion, reportedVer)
	}
}

// doShadowPost posts a JSON body to the shadow endpoint with the service
// token bearer header and returns the HTTP status. Fails the test on
// transport error.
func doShadowPost(t *testing.T, url, token string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// queryTemplateVersionDirect reads thing_config_template.version without
// going through the testHarness helpers — this test does not construct a
// testHarness (it builds a bare hub + fault wrapper) so it queries the
// store pool directly.
func queryTemplateVersionDirect(
	t *testing.T,
	hub *testharness.Harness,
	thingType, configKey string,
) (int64, bool) {
	t.Helper()
	var v int64
	err := hub.Store().Pool().QueryRow(context.Background(),
		`SELECT version FROM thing_config_template WHERE type=$1 AND config_key=$2`,
		thingType, configKey,
	).Scan(&v)
	if err != nil {
		// pgx.ErrNoRows is the "not found" case — return (0, false) so
		// callers can assert absence without a hard failure.
		if err.Error() == "no rows in result set" {
			return 0, false
		}
		t.Fatalf("queryTemplateVersionDirect: %v", err)
	}
	return v, true
}
