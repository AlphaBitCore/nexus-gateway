//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/testharness"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// testHarness wraps a testharness.Harness plus an httptest.Server so e2e tests
// can talk to the Hub over real HTTP and WebSocket transports. All state lives
// in Postgres (pointed at by DATABASE_URL); the harness process is ephemeral.
type testHarness struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	hub       *testharness.Harness
	ts        *httptest.Server
	logger    *slog.Logger
	hubHTTP   string // e.g. http://127.0.0.1:nnnn
	hubWSBase string // e.g. ws://127.0.0.1:nnnn (no path)
}

// newTestHarness boots a Hub harness wired to Postgres plus an httptest.Server
// serving the Echo handler. Transport teardown is registered via t.Cleanup,
// so callers do not need to defer anything themselves. Postgres cleanup is
// attached by testharness.NewForTest and by the individual register/issue
// helpers.
//
// Requires DATABASE_URL set; tests that reach this helper already gate on
// RUN_E2E=1 before calling.
func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	hub := testharness.NewForTest(t)
	ts := httptest.NewServer(hub.Handler())
	logger := newTestLogger()

	h := &testHarness{
		t:         t,
		ctx:       ctx,
		cancel:    cancel,
		hub:       hub,
		ts:        ts,
		logger:    logger,
		hubHTTP:   ts.URL,
		hubWSBase: "ws" + ts.URL[len("http"):],
	}
	t.Cleanup(func() {
		ts.Close()
		cancel()
	})
	return h
}

// newTestLogger returns a slog.Logger that writes to stderr at debug level.
// Shared between newTestHarness and connectFakeClient so both the harness
// and its fake clients surface the same log format.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// registerThing pre-creates a Thing row of the given type and returns its ID,
// enrollment (device) token, and the WebSocket URL the thingclient should use
// to connect. The row is cleaned up by the harness via t.Cleanup.
func (h *testHarness) registerThing(t *testing.T, thingID, thingType string) (id, token, wsURL string) {
	t.Helper()
	token = h.hub.IssueEnrollmentTokenOfType(t, thingID, thingType)
	wsURL = fmt.Sprintf("%s/ws?id=%s&type=%s", h.hubWSBase, thingID, thingType)
	return thingID, token, wsURL
}

// connectFakeClient creates a thingclient, wires an OnConfigChanged callback
// that invokes the apply function for every key in the delta, starts the
// client, and waits until the WebSocket handshake has completed. Callers MUST
// defer client.Close(ctx) so the underlying connection drains before the test
// exits.
//
// The apply callback MUST be safe for concurrent use; thingclient calls it
// from its internal goroutine whenever the Hub pushes a delta.
//
// The lifetime of the client is bound to h.ctx — the harness's context — so
// the WebSocket session survives for the whole test. Tests must not call
// close() on the harness until they are done with the client.
func (h *testHarness) connectFakeClient(
	t *testing.T,
	wsURL, thingID, thingType, token string,
	apply func(key string, cs thingclient.ConfigState) error,
) *thingclient.Client {
	t.Helper()

	logger := newTestLogger()

	// Each thingclient registers its own Prometheus collectors; share the
	// process-global default registerer across clients and promauto panics
	// with "duplicate metrics collector registration attempted". Allocate a
	// fresh isolated registry per client so tests can stand up multiple
	// thingclients in the same process.
	c, err := thingclient.New(thingclient.Config{
		HubURL:            wsURL,
		HubHTTPURL:        h.hubHTTP,
		ThingType:         thingType,
		ThingID:           thingID,
		Token:             token,
		Logger:            logger,
		MetricsRegisterer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("thingclient.New: %v", err)
	}

	c.OnConfigChanged(func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		out := make(map[string]thingclient.ConfigState, len(desired))
		for k, cs := range desired {
			if err := apply(k, cs); err != nil {
				return nil, err
			}
			out[k] = cs
		}
		return out, nil
	})

	if err := c.Start(h.ctx); err != nil {
		t.Fatalf("thingclient.Start: %v", err)
	}

	// Wait for the WebSocket handshake to complete so subsequent
	// notifyConfigChange broadcasts are not missed (connects arriving after
	// the broadcast would still get the full desired on the "connected"
	// message, but tests expect the delta path).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Mode() == thingclient.ModeWSConnected {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("thingclient did not reach WSConnected in 5s (mode=%s)", c.Mode())
	return c
}

// notifyConfigChange simulates a Control Plane admin handler calling
// /api/hub/config/update. It uses the harness's internal service token so
// ServiceAuth accepts the request. ActorID is always testharness.E2EActorID so
// the harness's cleanup hook drops the inserted config_change_event row(s).
func (h *testHarness) notifyConfigChange(
	t *testing.T,
	thingType, configKey string,
	state any,
	action string,
) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"thingType": thingType,
		"configKey": configKey,
		"state":     state,
		"action":    action,
		"actorId":   testharness.E2EActorID,
		"actorName": "e2e-harness",
		"sourceIp":  "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("notifyConfigChange: marshal: %v", err)
	}

	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost,
		h.hubHTTP+"/api/hub/config/update", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("notifyConfigChange: new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.hub.ServiceToken())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("notifyConfigChange: do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("notifyConfigChange: status=%d", resp.StatusCode)
	}
}

// changeEventRow holds the fields we assert on in config_change_event.
type changeEventRow struct {
	Action            string
	ActorID           string
	ActorName         string
	SourceIP          string
	NewVersion        int64
	EmergencyOverride bool
}

// queryLatestChangeEvent returns the most recent config_change_event row for
// (thingType, configKey). Fails the test if no row exists.
func (h *testHarness) queryLatestChangeEvent(t *testing.T, thingType, configKey string) changeEventRow {
	t.Helper()
	var r changeEventRow
	err := h.hub.Store().Pool().QueryRow(h.ctx,
		`SELECT action, actor_id, actor_name, COALESCE(source_ip, ''), new_version, emergency_override
		 FROM config_change_event
		 WHERE thing_type = $1 AND config_key = $2
		 ORDER BY timestamp DESC
		 LIMIT 1`,
		thingType, configKey,
	).Scan(&r.Action, &r.ActorID, &r.ActorName, &r.SourceIP, &r.NewVersion, &r.EmergencyOverride)
	if err != nil {
		t.Fatalf("queryLatestChangeEvent(%s/%s): %v", thingType, configKey, err)
	}
	return r
}

// queryTemplateVersion returns the current version of (type, config_key) in
// thing_config_template. If the row is missing, returns (0, false) without
// failing so break-glass tests can assert absence.
func (h *testHarness) queryTemplateVersion(t *testing.T, thingType, configKey string) (int64, bool) {
	t.Helper()
	var v int64
	err := h.hub.Store().Pool().QueryRow(h.ctx,
		`SELECT version FROM thing_config_template WHERE type = $1 AND config_key = $2`,
		thingType, configKey,
	).Scan(&v)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, false
		}
		t.Fatalf("queryTemplateVersion(%s/%s): %v", thingType, configKey, err)
	}
	return v, true
}

// sendBreakGlassReportHTTP posts to the dedicated break-glass route
// /api/internal/things/shadow/break-glass using the service token — the same
// path the production thingclient HTTP fallback posts to. Payload fields follow
// manager.ShadowReportRequest (id / reported / reportedVer / keyVersions /
// sourceIp / actorTokenId). The route itself is the break-glass signal; the
// handler stamps reason="break_glass".
func (h *testHarness) sendBreakGlassReportHTTP(t *testing.T, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("sendBreakGlassReportHTTP: marshal: %v", err)
	}

	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost,
		h.hubHTTP+"/api/internal/things/shadow/break-glass", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sendBreakGlassReportHTTP: new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.hub.ServiceToken())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sendBreakGlassReportHTTP: do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sendBreakGlassReportHTTP: status=%d", resp.StatusCode)
	}
}

// cleanupConfigState registers a t.Cleanup hook that removes the
// (type, config_key) row from thing_config_template and any associated
// config_change_event rows. Tests that bump a seeded template call this so
// re-runs start from a clean slate; the next notify will re-seed the row.
func (h *testHarness) cleanupConfigState(t *testing.T, thingType, configKey string) {
	t.Helper()
	t.Cleanup(func() {
		cleanCtx := context.Background()
		pool := h.hub.Store().Pool()
		_, _ = pool.Exec(cleanCtx,
			`DELETE FROM config_change_event WHERE thing_type = $1 AND config_key = $2`,
			thingType, configKey)
		_, _ = pool.Exec(cleanCtx,
			`DELETE FROM thing_config_template WHERE type = $1 AND config_key = $2`,
			thingType, configKey)
	})
}

// waitUntil polls cond every 20ms until it returns true or timeout elapses.
// Fails the test on timeout with a descriptive message.
func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitUntil(%s): condition not met within %s", desc, timeout)
}
