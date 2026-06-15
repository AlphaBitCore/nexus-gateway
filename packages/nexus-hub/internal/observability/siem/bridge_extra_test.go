package siem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// bridge_extra_test.go drives every DB-bound code path in bridge.go (Reload,
// Poll, loadCheckpoint, saveCheckpoint, queryEvents, queryAdminEvents) plus
// the HTTP sink contract in sink.go through pgxmock and httptest. The
// existing bridge_test.go covers the in-memory NewBridge + classify glue;
// this file lifts the package above the 95% statement coverage threshold
// without a live PostgreSQL.
//
// Per binding [[tests-only-own-data]]: these tests own zero real rows
// (pgxmock + httptest only) and therefore cannot violate the no-cross-test-
// data rule.

// newBridgeForTest constructs a Bridge against a pgxmock pool via the
// package-private newBridgeWithPool seam. Returns the bridge + the mock so
// individual tests can stack ExpectQuery / ExpectExec calls.
func newBridgeForTest(t *testing.T, sink Sink, cfg BridgeConfig) (*Bridge, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	b := newBridgeWithPool(mock, sink, cfg, testLogger())
	return b, mock
}

// captureSink records every batch Send was asked to deliver, plus an
// optional error to return so tests can drive the Send-failure branch.
type captureSink struct {
	name    string
	batches [][]Event
	err     error
}

func (c *captureSink) Name() string { return c.name }
func (c *captureSink) Send(_ context.Context, events []Event) error {
	cp := make([]Event, len(events))
	copy(cp, events)
	c.batches = append(c.batches, cp)
	return c.err
}

// PollInterval / ActiveSinkName getters

// TestPollInterval_DefaultsWhenUnset asserts the 30s fallback fires when
// activeCfg is nil — guards the scheduler against panicking on a partially-
// constructed bridge.
func TestPollInterval_DefaultsWhenUnset(t *testing.T) {
	b := NewBridge(nil, nil, BridgeConfig{}, testLogger())
	if got := b.PollInterval(); got != 30*time.Second {
		t.Errorf("PollInterval = %v, want 30s", got)
	}
	// Force the nil-snapshot branch by clearing the atomic pointer that
	// NewBridge populated. PollInterval must still return 30s.
	b.activeCfg.Store(nil)
	if got := b.PollInterval(); got != 30*time.Second {
		t.Errorf("PollInterval after clear = %v, want 30s", got)
	}
}

// TestPollInterval_HonorsConfig asserts the live snapshot value is returned.
func TestPollInterval_HonorsConfig(t *testing.T) {
	b := NewBridge(nil, nil, BridgeConfig{PollInterval: 5 * time.Second}, testLogger())
	if got := b.PollInterval(); got != 5*time.Second {
		t.Errorf("PollInterval = %v, want 5s", got)
	}
}

// TestActiveSinkName_EmptyWhenDisabled covers the nil-pointer branch.
func TestActiveSinkName_EmptyWhenDisabled(t *testing.T) {
	b := NewBridge(nil, nil, BridgeConfig{}, testLogger())
	if got := b.ActiveSinkName(); got != "" {
		t.Errorf("ActiveSinkName with nil sink = %q, want empty", got)
	}
}

// TestActiveSinkName_ReturnsSinkName covers the populated-pointer branch.
func TestActiveSinkName_ReturnsSinkName(t *testing.T) {
	sink := &captureSink{name: "test-sink"}
	b := NewBridge(nil, sink, BridgeConfig{}, testLogger())
	if got := b.ActiveSinkName(); got != "test-sink" {
		t.Errorf("ActiveSinkName = %q, want %q", got, "test-sink")
	}
}

// setIfNotNil / setIntIfNotNil helpers

// TestSetHelpers_OmitNil pins the contract that nil pointers are skipped
// entirely (so the outgoing event omits the field rather than including a
// JSON null) and that non-nil values land at the named key.
func TestSetHelpers_OmitNil(t *testing.T) {
	evt := Event{}
	setIfNotNil(evt, "skip", nil)
	if _, ok := evt["skip"]; ok {
		t.Errorf("setIfNotNil with nil should not set key")
	}
	v := "value"
	setIfNotNil(evt, "keep", &v)
	if evt["keep"] != "value" {
		t.Errorf("setIfNotNil(non-nil) = %v, want 'value'", evt["keep"])
	}

	setIntIfNotNil(evt, "skip2", nil)
	if _, ok := evt["skip2"]; ok {
		t.Errorf("setIntIfNotNil with nil should not set key")
	}
	n := 42
	setIntIfNotNil(evt, "keep2", &n)
	if evt["keep2"] != 42 {
		t.Errorf("setIntIfNotNil(non-nil) = %v, want 42", evt["keep2"])
	}
}

// TestReload_RowMissing_ClearsSink asserts pgx.ErrNoRows collapses the
// active sink to nil so subsequent Poll calls are no-ops (intentional "SIEM
// off" state).
func TestReload_RowMissing_ClearsSink(t *testing.T) {
	sink := &captureSink{name: "preexisting"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{})
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").
		WillReturnError(pgx.ErrNoRows)

	if err := b.Reload(context.Background()); err != nil {
		t.Fatalf("Reload returned err on ErrNoRows: %v", err)
	}
	if b.activeSink.Load() != nil {
		t.Errorf("active sink should be nil after ErrNoRows")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestReload_QueryError_Wrapped asserts a non-ErrNoRows pg error is wrapped
// and returned without nilling the sink (so the bridge keeps using the
// previous good config until the next successful Reload).
func TestReload_QueryError_Wrapped(t *testing.T) {
	sink := &captureSink{name: "preexisting"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{})
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("siem.config").
		WillReturnError(errors.New("connection refused"))

	err := b.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read siem.config") {
		t.Errorf("expected wrapped read error, got: %v", err)
	}
	if b.activeSink.Load() == nil {
		t.Errorf("active sink should NOT be cleared on transient pg error")
	}
}

// TestReload_UnmarshalError asserts malformed JSON in the config row
// surfaces as a wrapped "parse siem.config" error.
func TestReload_UnmarshalError(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(json.RawMessage(`{"enabled": "not-a-bool"`)))

	err := b.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "parse siem.config") {
		t.Errorf("expected wrapped parse error, got: %v", err)
	}
}

// TestReload_DisabledOrEmptyURL_ClearsSink asserts Enabled=false OR URL=""
// both collapse the sink to the disabled state.
func TestReload_DisabledOrEmptyURL_ClearsSink(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"enabled false", `{"enabled": false, "url": "https://siem.example.com"}`},
		{"url empty", `{"enabled": true, "url": ""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureSink{name: "preexisting"}
			b, mock := newBridgeForTest(t, sink, BridgeConfig{})
			mock.ExpectQuery(`SELECT value FROM system_metadata`).
				WithArgs("siem.config").
				WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(json.RawMessage(tc.raw)))

			if err := b.Reload(context.Background()); err != nil {
				t.Fatalf("Reload: %v", err)
			}
			if b.activeSink.Load() != nil {
				t.Errorf("active sink should be nil for %s", tc.name)
			}
		})
	}
}

// TestReload_BuildsSink_Defaults asserts an enabled row with no interval /
// batch fields gets default 30s / 200 applied to the new live cfg.
func TestReload_BuildsSink_Defaults(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	raw := `{"enabled": true, "url": "https://siem.example.com", "format": "json"}`
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(json.RawMessage(raw)))

	if err := b.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	sinkPtr := b.activeSink.Load()
	if sinkPtr == nil {
		t.Fatal("activeSink nil after successful Reload")
	}
	if name := (*sinkPtr).Name(); !strings.Contains(name, "siem.example.com") {
		t.Errorf("sink name = %q, want it to contain the configured URL", name)
	}
	cfg := b.activeCfg.Load()
	if cfg == nil || cfg.PollInterval != 30*time.Second {
		t.Errorf("PollInterval = %v, want 30s default", cfg.PollInterval)
	}
	if cfg.BatchSize != 200 {
		t.Errorf("BatchSize = %d, want 200 default", cfg.BatchSize)
	}
}

// TestReload_BuildsSink_HonorsConfig pins that non-default interval / batch
// values flow through into the live snapshot.
func TestReload_BuildsSink_HonorsConfig(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	raw := `{"enabled": true, "url": "https://siem.example.com", "format": "cef", "pollIntervalSeconds": 60, "batchSize": 500, "eventTypes": ["traffic.allowed"]}`
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(json.RawMessage(raw)))

	if err := b.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	cfg := b.activeCfg.Load()
	if cfg.PollInterval != 60*time.Second {
		t.Errorf("PollInterval = %v, want 60s", cfg.PollInterval)
	}
	if cfg.BatchSize != 500 {
		t.Errorf("BatchSize = %d, want 500", cfg.BatchSize)
	}
	if len(cfg.EventTypes) != 1 || cfg.EventTypes[0] != "traffic.allowed" {
		t.Errorf("EventTypes = %v, want [traffic.allowed]", cfg.EventTypes)
	}
}

// TestLoadCheckpoint_NoRow_Returns24hAgo asserts the genesis-window default
// kicks in when the checkpoint row is absent (first-ever Poll).
func TestLoadCheckpoint_NoRow_Returns24hAgo(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).
		WillReturnError(pgx.ErrNoRows)

	cp, err := b.loadCheckpoint(context.Background(), checkpointKey)
	if err != nil {
		t.Fatalf("loadCheckpoint: %v", err)
	}
	since := time.Since(cp.TS)
	if since < 23*time.Hour || since > 25*time.Hour {
		t.Errorf("expected ~24h ago, got delta %v", since)
	}
	if cp.ID != "" {
		t.Errorf("cold-start cursor id = %q, want empty", cp.ID)
	}
}

// TestLoadCheckpoint_QueryError_Wrapped asserts non-ErrNoRows errors are
// surfaced (so Poll bails before sending stale data).
func TestLoadCheckpoint_QueryError_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(checkpointKey).
		WillReturnError(errors.New("boom"))

	_, err := b.loadCheckpoint(context.Background(), checkpointKey)
	if err == nil || !strings.Contains(err.Error(), "load checkpoint") {
		t.Errorf("expected wrapped err, got: %v", err)
	}
}

// TestLoadCheckpoint_KeysetForm parses the canonical keyset-cursor object
// saveCheckpoint writes ({"ts":...,"id":...}) and round-trips both fields.
func TestLoadCheckpoint_KeysetForm(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	wantTS := time.Date(2026, 5, 17, 12, 30, 0, 0, time.UTC)
	value, _ := json.Marshal(bridgeCheckpoint{TS: wantTS, ID: "evt-42"})
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(checkpointKey).
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(json.RawMessage(value)))

	got, err := b.loadCheckpoint(context.Background(), checkpointKey)
	if err != nil {
		t.Fatalf("loadCheckpoint: %v", err)
	}
	if !got.TS.Equal(wantTS) || got.ID != "evt-42" {
		t.Errorf("loadCheckpoint = %+v, want ts=%v id=evt-42", got, wantTS)
	}
}

// TestLoadCheckpoint_Unparseable_Resets24h asserts a garbage row falls back
// to the 24h-ago default (so a corrupted row doesn't brick the bridge).
func TestLoadCheckpoint_Unparseable_Resets24h(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(checkpointKey).
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(json.RawMessage(`12345`)))

	cp, err := b.loadCheckpoint(context.Background(), checkpointKey)
	if err != nil {
		t.Fatalf("loadCheckpoint: %v", err)
	}
	since := time.Since(cp.TS)
	if since < 23*time.Hour || since > 25*time.Hour {
		t.Errorf("unparseable row should default to ~24h ago, got %v", since)
	}
}

// TestSaveCheckpoint_Success pins the upsert SQL shape + that the persisted
// value is the keyset-cursor JSON object (canonical form).
func TestSaveCheckpoint_Success(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	cp := bridgeCheckpoint{TS: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC), ID: "evt-7"}
	expectedValue, _ := json.Marshal(cp)

	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(checkpointKey, expectedValue).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	if err := b.saveCheckpoint(context.Background(), checkpointKey, cp); err != nil {
		t.Fatalf("saveCheckpoint: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestSaveCheckpoint_ExecError_Wrapped asserts pg errors are surfaced to the
// Poll caller (so it can log without retrying out of order).
func TestSaveCheckpoint_ExecError_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	cp := bridgeCheckpoint{TS: time.Now().UTC(), ID: "evt-x"}
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("deadlock detected"))

	err := b.saveCheckpoint(context.Background(), checkpointKey, cp)
	if err == nil || !strings.Contains(err.Error(), "save checkpoint") {
		t.Errorf("expected wrapped save error, got: %v", err)
	}
}

// trafficRows returns a pgxmock Rows preloaded with two synthetic traffic
// rows — one allowed (both hook stages NULL), one blocked at the request
// stage. Used by queryEvents tests.
func trafficRowsTwo() *pgxmock.Rows {
	srcIP := "10.0.0.1"
	host := "api.example.com"
	method := "POST"
	path := "/v1/chat/completions"
	status := 200
	latency := 42
	entity := "vk-1"
	entityType := "virtualKey"
	org := "org-1"
	reqDecision := "block"
	reqReason := "rate limit"
	reqCode := "rate_limited"
	details := json.RawMessage(`{"model":"gpt-4o"}`)
	tags := []string{"pii"}
	trace := "req-abc123"

	rows := pgxmock.NewRows([]string{
		"id", "source", "timestamp",
		"sourceIp", "targetHost", "method", "path", "statusCode", "latencyMs",
		"entityId", "entityType", "orgId",
		"reqDecision", "reqReason", "reqCode",
		"respDecision", "respReason", "respCode",
		"complianceTags", "details", "traceId",
	})
	// Row 1 — allowed (all hook fields NULL — back-compat alias copies nothing);
	// trace_id present so the forwarded event carries the correlation key.
	rows.AddRow(
		"evt-1", "ai-gateway", time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC),
		&srcIP, &host, &method, &path, &status, &latency,
		&entity, &entityType, &org,
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		tags, &details, &trace,
	)
	// Row 2 — blocked at request stage; targetHost / details / trace_id NULL so
	// the nil-omit branches fire.
	rows.AddRow(
		"evt-2", "ai-gateway", time.Date(2026, 5, 17, 10, 0, 1, 0, time.UTC),
		&srcIP, (*string)(nil), &method, &path, &status, &latency,
		&entity, &entityType, &org,
		&reqDecision, &reqReason, &reqCode,
		(*string)(nil), (*string)(nil), (*string)(nil),
		[]string(nil), (*json.RawMessage)(nil), (*string)(nil),
	)
	return rows
}

// TestQueryEvents_SecurityMode builds the security-mode SQL and decodes a
// two-row result.
func TestQueryEvents_SecurityMode(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	since := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`request_hook_decision = 'block'`).
		WithArgs(since, "", 50).
		WillReturnRows(trafficRowsTwo())

	events, next, err := b.queryEvents(context.Background(), bridgeCheckpoint{TS: since}, 50)
	if err != nil {
		t.Fatalf("queryEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if next.TS.IsZero() {
		t.Errorf("next cursor TS should be set from last row")
	}
	// Row 1 — allowed: no request/response hook fields present
	if _, ok := events[0]["hookDecision"]; ok {
		t.Errorf("row 1 should not have hookDecision (both stages nil)")
	}
	// Row 2 — blocked: back-compat alias must surface request stage when
	// response stage is nil.
	if events[1]["hookDecision"] != "block" {
		t.Errorf("row 2 hookDecision = %v, want 'block'", events[1]["hookDecision"])
	}
	if events[1]["hookReasonCode"] != "rate_limited" {
		t.Errorf("row 2 hookReasonCode = %v, want 'rate_limited'", events[1]["hookReasonCode"])
	}
	// nil-omit branch: row 2's targetHost was NULL, so the field is absent.
	if _, ok := events[1]["targetHost"]; ok {
		t.Errorf("row 2 should omit targetHost (NULL pointer)")
	}
	// row 1's complianceTags + details populated.
	if tags, ok := events[0]["complianceTags"].([]string); !ok || len(tags) != 1 {
		t.Errorf("row 1 complianceTags = %v, want 1 string slice", events[0]["complianceTags"])
	}
	if _, ok := events[0]["details"]; !ok {
		t.Errorf("row 1 should have parsed details")
	}
	// row 1 carries the trace_id correlation key; row 2's was NULL so it is omitted.
	if events[0]["traceId"] != "req-abc123" {
		t.Errorf("row 1 traceId = %v, want 'req-abc123'", events[0]["traceId"])
	}
	if _, ok := events[1]["traceId"]; ok {
		t.Errorf("row 2 should omit traceId (NULL pointer)")
	}
}

// TestQueryEvents_ResponseHookOverridesRequest exercises the back-compat
// alias precedence: when both stages have a hook decision, response wins.
func TestQueryEvents_ResponseHookOverridesRequest(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})

	srcIP := "10.0.0.2"
	method := "POST"
	path := "/v1/messages"
	reqDecision := "allow"
	reqReason := "allowed by rulepack"
	reqCode := "ok"
	respDecision := "block"
	respReason := "pii leak"
	respCode := "pii_redacted"

	rows := pgxmock.NewRows([]string{
		"id", "source", "timestamp",
		"sourceIp", "targetHost", "method", "path", "statusCode", "latencyMs",
		"entityId", "entityType", "orgId",
		"reqDecision", "reqReason", "reqCode",
		"respDecision", "respReason", "respCode",
		"complianceTags", "details", "traceId",
	}).AddRow(
		"evt-3", "ai-gateway", time.Now().UTC(),
		&srcIP, (*string)(nil), &method, &path, (*int)(nil), (*int)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		&reqDecision, &reqReason, &reqCode,
		&respDecision, &respReason, &respCode,
		[]string(nil), (*json.RawMessage)(nil), (*string)(nil),
	)
	mock.ExpectQuery(`request_hook_decision = 'block'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	events, _, err := b.queryEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 10)
	if err != nil {
		t.Fatalf("queryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	// Back-compat alias must prefer response stage.
	if events[0]["hookDecision"] != "block" {
		t.Errorf("hookDecision = %v, want 'block' (response wins)", events[0]["hookDecision"])
	}
	if events[0]["hookReasonCode"] != "pii_redacted" {
		t.Errorf("hookReasonCode = %v, want 'pii_redacted'", events[0]["hookReasonCode"])
	}
	if events[0]["requestHookDecision"] != "allow" {
		t.Errorf("requestHookDecision should still be exposed = %v", events[0]["requestHookDecision"])
	}
}

// TestQueryEvents_InvalidDetailsJSON falls through the json.Unmarshal == nil
// guard — corrupted details column is silently omitted rather than crashing.
func TestQueryEvents_InvalidDetailsJSON(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	srcIP := "10.0.0.3"
	badDetails := json.RawMessage(`{"unterminated":`)
	rows := pgxmock.NewRows([]string{
		"id", "source", "timestamp",
		"sourceIp", "targetHost", "method", "path", "statusCode", "latencyMs",
		"entityId", "entityType", "orgId",
		"reqDecision", "reqReason", "reqCode",
		"respDecision", "respReason", "respCode",
		"complianceTags", "details", "traceId",
	}).AddRow(
		"evt-bad", "ai-gateway", time.Now().UTC(),
		&srcIP, (*string)(nil), (*string)(nil), (*string)(nil), (*int)(nil), (*int)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		[]string(nil), &badDetails, (*string)(nil),
	)
	mock.ExpectQuery(`request_hook_decision = 'block'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	events, _, err := b.queryEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 10)
	if err != nil {
		t.Fatalf("queryEvents: %v", err)
	}
	if _, ok := events[0]["details"]; ok {
		t.Errorf("bad details JSON should be silently omitted, got %v", events[0]["details"])
	}
}

// TestQueryEvents_QueryError_Wrapped covers the rows.Query() failure branch.
func TestQueryEvents_QueryError_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("relation does not exist"))

	_, _, err := b.queryEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 10)
	if err == nil || !strings.Contains(err.Error(), "query traffic_event") {
		t.Errorf("expected wrapped query err, got: %v", err)
	}
}

// TestQueryEvents_ScanError_Wrapped covers the rows.Scan() failure branch by
// supplying a row whose column count doesn't match the target tuple.
func TestQueryEvents_ScanError_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	// Only 2 columns when the bridge expects 21 — scan must fail.
	rows := pgxmock.NewRows([]string{"id", "source"}).AddRow("evt", "src")
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	_, _, err := b.queryEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 10)
	if err == nil || !strings.Contains(err.Error(), "scan traffic_event") {
		t.Errorf("expected wrapped scan err, got: %v", err)
	}
}

// TestQueryEvents_RowsErr_Wrapped covers the rows.Err() iteration failure
// branch by attaching a CloseError to the rows iterator — pgxmock surfaces
// that as the rows.Err() result after the visible rows finish.
func TestQueryEvents_RowsErr_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	srcIP := "10.0.0.4"
	rows := pgxmock.NewRows([]string{
		"id", "source", "timestamp",
		"sourceIp", "targetHost", "method", "path", "statusCode", "latencyMs",
		"entityId", "entityType", "orgId",
		"reqDecision", "reqReason", "reqCode",
		"respDecision", "respReason", "respCode",
		"complianceTags", "details", "traceId",
	}).AddRow(
		"evt-x", "ai-gateway", time.Now().UTC(),
		&srcIP, (*string)(nil), (*string)(nil), (*string)(nil), (*int)(nil), (*int)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		[]string(nil), (*json.RawMessage)(nil), (*string)(nil),
	).CloseError(errors.New("network blip post-iteration"))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	_, _, err := b.queryEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 10)
	if err == nil || !strings.Contains(err.Error(), "rows iteration") {
		t.Errorf("expected wrapped rows iteration err, got: %v", err)
	}
}

// adminRowsOne returns a pgxmock Rows with one populated admin row + one row
// with NULL beforeState/afterState/actor* to exercise both branches.
func adminRowsTwo() *pgxmock.Rows {
	actor := "user-1"
	label := "alice"
	role := "super-admin"
	ip := "10.0.0.5"
	entityID := "vk-42"
	before := json.RawMessage(`{"old":"v"}`)
	after := json.RawMessage(`{"new":"v"}`)

	via := "assistant"

	rows := pgxmock.NewRows([]string{
		"id", "timestamp",
		"actorId", "actorLabel", "actorRole",
		"sourceIp", "action", "entityType", "entityId",
		"beforeState", "afterState", "via",
	})
	rows.AddRow(
		"adm-1", time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC),
		&actor, &label, &role,
		&ip, "create", "virtual-key", &entityID,
		&before, &after, &via,
	)
	// Row 2 — all optional fields NULL (incl. via) — exercises the nil-omit branches.
	rows.AddRow(
		"adm-2", time.Date(2026, 5, 17, 9, 1, 0, 0, time.UTC),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), "delete", "session", (*string)(nil),
		(*json.RawMessage)(nil), (*json.RawMessage)(nil), (*string)(nil),
	)
	return rows
}

// TestQueryAdminEvents_PopulatesFields drives a full decode + asserts the
// nil-omit branches drop missing actor / entity / state fields and the
// non-nil branches surface the values.
func TestQueryAdminEvents_PopulatesFields(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	since := time.Date(2026, 5, 17, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(since, "", 100).
		WillReturnRows(adminRowsTwo())

	events, next, err := b.queryAdminEvents(context.Background(), bridgeCheckpoint{TS: since}, 100)
	if err != nil {
		t.Fatalf("queryAdminEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	// Row 1 — populated.
	if events[0]["actorLabel"] != "alice" {
		t.Errorf("actorLabel = %v, want 'alice'", events[0]["actorLabel"])
	}
	if events[0]["action"] != "create" {
		t.Errorf("action = %v, want 'create'", events[0]["action"])
	}
	if _, ok := events[0]["beforeState"]; !ok {
		t.Errorf("beforeState should be parsed and present")
	}
	// Row 1 — via="assistant" must be exported so the SIEM can distinguish the
	// AI-initiated write from a human one (E90 I5 — the whole point of the column).
	if events[0]["via"] != "assistant" {
		t.Errorf("row 1 via = %v, want 'assistant'", events[0]["via"])
	}
	// Row 2 — nil fields omitted.
	if _, ok := events[1]["actorLabel"]; ok {
		t.Errorf("row 2 actorLabel should be omitted (was NULL)")
	}
	if _, ok := events[1]["beforeState"]; ok {
		t.Errorf("row 2 beforeState should be omitted (was NULL)")
	}
	if _, ok := events[1]["via"]; ok {
		t.Errorf("row 2 via should be omitted (human write, was NULL)")
	}
	if next.TS.IsZero() {
		t.Errorf("next cursor TS should be set")
	}
	// source field is unconditionally stamped to "admin".
	if events[0]["source"] != "admin" {
		t.Errorf("source = %v, want 'admin'", events[0]["source"])
	}
}

// TestQueryAdminEvents_InvalidStateJSON skips JSON-unmarshal failures on
// beforeState / afterState columns (silently omits rather than failing).
func TestQueryAdminEvents_InvalidStateJSON(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	bad := json.RawMessage(`{`)
	rows := pgxmock.NewRows([]string{
		"id", "timestamp",
		"actorId", "actorLabel", "actorRole",
		"sourceIp", "action", "entityType", "entityId",
		"beforeState", "afterState", "via",
	}).AddRow(
		"adm-bad", time.Now().UTC(),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), "update", "thing", (*string)(nil),
		&bad, &bad, (*string)(nil),
	)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	events, _, err := b.queryAdminEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 100)
	if err != nil {
		t.Fatalf("queryAdminEvents: %v", err)
	}
	if _, ok := events[0]["beforeState"]; ok {
		t.Errorf("bad beforeState JSON should be silently omitted")
	}
	if _, ok := events[0]["afterState"]; ok {
		t.Errorf("bad afterState JSON should be silently omitted")
	}
}

// TestQueryAdminEvents_QueryError_Wrapped covers the rows.Query() failure.
func TestQueryAdminEvents_QueryError_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("conn closed"))

	_, _, err := b.queryAdminEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 100)
	if err == nil || !strings.Contains(err.Error(), "query AdminAuditLog") {
		t.Errorf("expected wrapped err, got: %v", err)
	}
}

// TestQueryAdminEvents_ScanError_Wrapped covers the row Scan failure branch.
func TestQueryAdminEvents_ScanError_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	rows := pgxmock.NewRows([]string{"id", "timestamp"}).AddRow("a", time.Now())
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	_, _, err := b.queryAdminEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 100)
	if err == nil || !strings.Contains(err.Error(), "scan AdminAuditLog") {
		t.Errorf("expected wrapped scan err, got: %v", err)
	}
}

// TestQueryAdminEvents_RowsErr_Wrapped covers the rows.Err() failure
// branch via CloseError — see TestQueryEvents_RowsErr_Wrapped.
func TestQueryAdminEvents_RowsErr_Wrapped(t *testing.T) {
	b, mock := newBridgeForTest(t, nil, BridgeConfig{})
	rows := pgxmock.NewRows([]string{
		"id", "timestamp",
		"actorId", "actorLabel", "actorRole",
		"sourceIp", "action", "entityType", "entityId",
		"beforeState", "afterState", "via",
	}).AddRow(
		"adm-iter", time.Now().UTC(),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), "create", "x", (*string)(nil),
		(*json.RawMessage)(nil), (*json.RawMessage)(nil), (*string)(nil),
	).CloseError(errors.New("iter blip post-iteration"))
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	_, _, err := b.queryAdminEvents(context.Background(), bridgeCheckpoint{TS: time.Now()}, 100)
	if err == nil || !strings.Contains(err.Error(), "rows iteration (AdminAuditLog)") {
		t.Errorf("expected wrapped rows iteration err, got: %v", err)
	}
}

// Poll — full integration through pgxmock + captureSink

// TestPoll_FullSuccessCycle drives Poll end-to-end: Reload (enabled row),
// both checkpoint loads, both queries (traffic + admin), Send (against the
// HTTPSink Reload built — verified via httptest), and both checkpoint
// saves.
func TestPoll_FullSuccessCycle(t *testing.T) {
	allowLoopbackSink(t)
	// Capture every Send the Reload-built HTTPSink delivers.
	var sendCount int32
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sendCount, 1)
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b, mock := newBridgeForTest(t, nil, BridgeConfig{})

	// Reload row points at our httptest server.
	cfgRow := map[string]any{
		"enabled":             true,
		"url":                 srv.URL,
		"format":              "json",
		"pollIntervalSeconds": 30,
		"batchSize":           200,
	}
	rawCfg, _ := json.Marshal(cfgRow)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(json.RawMessage(rawCfg)))

	// Traffic checkpoint — first call, no row → 24h default.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	// Traffic query — security-mode SQL (block / rate-limited / budget-exceeded).
	mock.ExpectQuery(`request_hook_decision = 'block'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(trafficRowsTwo())

	// Admin checkpoint — first call, no row.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
	// Admin query — return 2 admin events.
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(adminRowsTwo())

	// Two saveCheckpoint upserts — one per non-empty event set.
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
	if atomic.LoadInt32(&sendCount) != 1 {
		t.Errorf("expected exactly 1 Send (forwarded batch), got %d", sendCount)
	}
	// Body should contain at least one traffic id and one admin id.
	if !strings.Contains(string(capturedBody), "evt-1") || !strings.Contains(string(capturedBody), "adm-1") {
		t.Errorf("forwarded body missing expected event ids: %s", capturedBody)
	}
}

// TestPoll_DisabledShortCircuits asserts an empty siem.config (Enabled=false)
// nils out the sink and Poll exits before any checkpoint / query work.
func TestPoll_DisabledShortCircuits(t *testing.T) {
	sink := &captureSink{name: "preexisting"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{})
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").
		WillReturnError(pgx.ErrNoRows)

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations (only the reload select): %v", err)
	}
	if len(sink.batches) != 0 {
		t.Errorf("disabled Poll must not call Send, got %d batches", len(sink.batches))
	}
}

// TestPoll_ReloadError_KeepsPreviousSink asserts a transient Reload error
// is logged and Poll continues using whatever activeSink was last set.
// We exercise this by pre-storing the captureSink + activeCfg manually,
// then making the Reload SELECT fail mid-cycle.
func TestPoll_ReloadError_KeepsPreviousSink(t *testing.T) {
	sink := &captureSink{name: "warmed-sink"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	// Reload fails with a non-NoRows pg error. activeSink stays populated.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").
		WillReturnError(errors.New("conn refused"))
	// Both checkpoint loads — return ErrNoRows so the 24h-ago default fires.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	// One traffic row.
	srcIP := "10.0.0.10"
	rows := pgxmock.NewRows([]string{
		"id", "source", "timestamp",
		"sourceIp", "targetHost", "method", "path", "statusCode", "latencyMs",
		"entityId", "entityType", "orgId",
		"reqDecision", "reqReason", "reqCode",
		"respDecision", "respReason", "respCode",
		"complianceTags", "details", "traceId",
	}).AddRow(
		"evt-r1", "ai-gateway", time.Now().UTC(),
		&srcIP, (*string)(nil), (*string)(nil), (*string)(nil), (*int)(nil), (*int)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		[]string(nil), (*json.RawMessage)(nil), (*string)(nil),
	)
	mock.ExpectQuery(`request_hook_decision = 'block'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)
	// Admin checkpoint + admin query (empty result).
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "timestamp",
			"actorId", "actorLabel", "actorRole",
			"sourceIp", "action", "entityType", "entityId",
			"beforeState", "afterState", "via",
		}))
	// One saveCheckpoint for traffic (admin had 0 events so no save).
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
	if len(sink.batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(sink.batches))
	}
	if len(sink.batches[0]) != 1 {
		t.Errorf("expected 1 event in batch, got %d", len(sink.batches[0]))
	}
}

// TestPoll_TrafficCheckpointError_AbortsCycle asserts a non-ErrNoRows error
// loading the traffic checkpoint short-circuits Poll before any query runs.
func TestPoll_TrafficCheckpointError_AbortsCycle(t *testing.T) {
	sink := &captureSink{name: "ok"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	// Reload fails with a transient error — keeps the captureSink alive.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	// Traffic checkpoint load fails with a non-NoRows error.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(errors.New("table missing"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
	if len(sink.batches) != 0 {
		t.Errorf("aborted Poll must not call Send")
	}
}

// TestPoll_TrafficQueryError_AbortsCycle covers the abort-on-traffic-query
// failure branch.
func TestPoll_TrafficQueryError_AbortsCycle(t *testing.T) {
	sink := &captureSink{name: "ok"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("query exploded"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestPoll_AdminCheckpointError_AbortsCycle covers the admin checkpoint
// failure branch (after traffic query succeeded).
func TestPoll_AdminCheckpointError_AbortsCycle(t *testing.T) {
	sink := &captureSink{name: "ok"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(emptyTrafficRows())
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(errors.New("permission denied"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestPoll_AdminQueryError_AbortsCycle covers the admin query failure
// branch.
func TestPoll_AdminQueryError_AbortsCycle(t *testing.T) {
	sink := &captureSink{name: "ok"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(emptyTrafficRows())
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("admin query down"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestPoll_AllEventsFiltered_NoSend asserts when the eventType filter
// drops every row, Poll returns before invoking Send / saveCheckpoint.
func TestPoll_AllEventsFiltered_NoSend(t *testing.T) {
	sink := &captureSink{name: "ok"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{
		BatchSize:    200,
		PollInterval: 30 * time.Second,
		EventTypes:   []string{"never.matches"},
	})

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(trafficRowsTwo())
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(adminRowsTwo())

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
	if len(sink.batches) != 0 {
		t.Errorf("filtered-empty Poll must not call Send, got %d batches", len(sink.batches))
	}
}

// TestPoll_SendFailure_NoCheckpointSave asserts a Sink.Send failure aborts
// Poll without persisting either checkpoint (so the next tick retries the
// same rows).
func TestPoll_SendFailure_NoCheckpointSave(t *testing.T) {
	sink := &captureSink{name: "ok", err: errors.New("siem 503")}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(trafficRowsTwo())
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(adminRowsTwo())
	// No saveCheckpoint expectations — Poll must abort before them.

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations (no saves allowed): %v", err)
	}
	if len(sink.batches) != 1 {
		t.Errorf("expected Send to be invoked once, got %d", len(sink.batches))
	}
}

// TestPoll_SaveCheckpointError_LoggedNotPropagated asserts Poll continues
// past a saveCheckpoint Exec failure (just logs) so the second checkpoint
// is still attempted.
func TestPoll_SaveCheckpointError_LoggedNotPropagated(t *testing.T) {
	sink := &captureSink{name: "ok"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(trafficRowsTwo())
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(adminRowsTwo())
	// Traffic save fails; admin save still runs.
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("write-conflict"))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestPoll_AdminSaveCheckpointError_LoggedNotPropagated asserts the
// symmetrical case: the second saveCheckpoint failing logs but does not
// affect the Poll return.
func TestPoll_AdminSaveCheckpointError_LoggedNotPropagated(t *testing.T) {
	sink := &captureSink{name: "ok"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("siem.config").WillReturnError(errors.New("transient"))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(trafficRowsTwo())
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(adminRowsTwo())
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("write-conflict"))

	b.Poll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// emptyTrafficRows produces an empty result set with the bridge's expected
// column layout — used by the abort-path Poll tests where we want the
// admin checkpoint / query to be reached but no traffic events forwarded.
func emptyTrafficRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "source", "timestamp",
		"sourceIp", "targetHost", "method", "path", "statusCode", "latencyMs",
		"entityId", "entityType", "orgId",
		"reqDecision", "reqReason", "reqCode",
		"respDecision", "respReason", "respCode",
		"complianceTags", "details", "traceId",
	})
}

// HTTPSink (sink.go) — full surface via httptest

// TestNewHTTPSink_EmptyURL_Rejects pins the validation guard.
func TestNewHTTPSink_EmptyURL_Rejects(t *testing.T) {
	_, err := NewHTTPSink("", nil, &JSONFormatter{})
	if err == nil {
		t.Errorf("NewHTTPSink('') should error")
	}
}

// TestNewHTTPSink_NilFormatter_DefaultsToJSON asserts the constructor
// supplies the default JSONFormatter when callers don't pass one.
func TestNewHTTPSink_NilFormatter_DefaultsToJSON(t *testing.T) {
	s, err := NewHTTPSink("https://example.com/hec", map[string]string{"Authorization": "Splunk x"}, nil)
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	if s.formatter == nil {
		t.Errorf("formatter should default to JSONFormatter, got nil")
	}
	if _, ok := s.formatter.(*JSONFormatter); !ok {
		t.Errorf("default formatter is %T, want *JSONFormatter", s.formatter)
	}
	if s.client == nil {
		t.Errorf("client should be initialised")
	}
}

// TestHTTPSink_Name covers the trivial Name accessor.
func TestHTTPSink_Name(t *testing.T) {
	s, err := NewHTTPSink("https://example.com/x", nil, nil)
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	if name := s.Name(); name != "http:https://example.com/x" {
		t.Errorf("Name() = %q, want %q", name, "http:https://example.com/x")
	}
}

// TestHTTPSink_Send_2xx_OK asserts a 2xx response returns nil and that the
// allowLoopbackSink disables the production SSRF dial guard for the duration of
// a test so an in-process httptest server (which binds to 127.0.0.1) stays
// reachable. The guard's own block behaviour is covered by
// TestHTTPSink_RejectsPrivateURL; these tests target Send formatting / status
// handling, not the guard. The prior value is restored on cleanup.
func allowLoopbackSink(t *testing.T) {
	t.Helper()
	prev := httpSinkDialControl
	httpSinkDialControl = nil
	t.Cleanup(func() { httpSinkDialControl = prev })
}

// TestHTTPSink_RejectsPrivateURL is the SEC-M6-01 egress regression: with the
// production guard in force (no allowLoopbackSink), a sink whose URL resolves to
// a loopback / private address must fail to deliver — the dial is refused before
// any byte of the audit stream leaves the Hub. Without the guard this 127.0.0.1
// server would happily receive the batch.
func TestHTTPSink_RejectsPrivateURL(t *testing.T) {
	var delivered int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&delivered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := NewHTTPSink(srv.URL, nil, &JSONFormatter{})
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	err = s.Send(context.Background(), []Event{{"id": "secret-audit-row"}})
	if err == nil {
		t.Fatal("Send to a loopback URL must fail — the SSRF guard did not block it")
	}
	if !strings.Contains(err.Error(), "ssrf-guard") {
		t.Errorf("error = %v; want it to cite the ssrf-guard refusal", err)
	}
	if atomic.LoadInt32(&delivered) != 0 {
		t.Errorf("audit batch reached the loopback server %d time(s) — guard breached", delivered)
	}
}

// server saw the configured headers + Content-Type + JSON body.
func TestHTTPSink_Send_2xx_OK(t *testing.T) {
	allowLoopbackSink(t)
	var capturedHeader http.Header
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := NewHTTPSink(srv.URL, map[string]string{"Authorization": "Splunk test-token"}, &JSONFormatter{})
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	evt := Event{"id": "evt-1", "eventType": "traffic.allowed"}
	if err := s.Send(context.Background(), []Event{evt}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if got := capturedHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := capturedHeader.Get("Authorization"); got != "Splunk test-token" {
		t.Errorf("Authorization header missing/wrong = %q", got)
	}
	if !strings.Contains(string(capturedBody), "evt-1") {
		t.Errorf("body did not contain event id, got: %s", capturedBody)
	}
}

// TestHTTPSink_Send_NonJSONFormatter_ContentType pins that the CEFFormatter
// drives Content-Type=text/plain on the wire.
func TestHTTPSink_Send_NonJSONFormatter_ContentType(t *testing.T) {
	allowLoopbackSink(t)
	var ct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, _ := NewHTTPSink(srv.URL, nil, &CEFFormatter{})
	if err := s.Send(context.Background(), []Event{{"eventType": "iam.update"}}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain (CEF)", ct)
	}
}

// TestHTTPSink_Send_3xx_Errors asserts any 3xx+ status is surfaced as an
// error — the bridge logs + skips the checkpoint save on this branch.
func TestHTTPSink_Send_3xx_Errors(t *testing.T) {
	allowLoopbackSink(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s, _ := NewHTTPSink(srv.URL, nil, &JSONFormatter{})
	err := s.Send(context.Background(), []Event{{"id": "x"}})
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Errorf("expected status 401 wrapped err, got: %v", err)
	}
}

// TestHTTPSink_Send_NetworkError pins the transport-failure branch by
// pointing at a closed server.
func TestHTTPSink_Send_NetworkError(t *testing.T) {
	allowLoopbackSink(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // immediately closed — Do() should fail.

	s, _ := NewHTTPSink(srv.URL, nil, &JSONFormatter{})
	err := s.Send(context.Background(), []Event{{"id": "x"}})
	if err == nil || !strings.Contains(err.Error(), "post") {
		t.Errorf("expected wrapped post err, got: %v", err)
	}
}

// TestHTTPSink_Send_FormatError surfaces a Formatter failure as a wrapped
// "format" error before any HTTP work happens. We supply a failing
// formatter to drive the branch deterministically.
func TestHTTPSink_Send_FormatError(t *testing.T) {
	s := &HTTPSink{
		url:       "https://example.invalid/x",
		formatter: failFormatter{},
		client:    &http.Client{},
	}
	err := s.Send(context.Background(), []Event{{"id": "x"}})
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Errorf("expected wrapped format err, got: %v", err)
	}
}

// TestHTTPSink_Send_NewRequestError covers the http.NewRequestWithContext
// failure branch by supplying an invalid URL (control characters reject
// the URL parse). The Send call must surface a wrapped "new request" err
// and never touch the wire.
func TestHTTPSink_Send_NewRequestError(t *testing.T) {
	s := &HTTPSink{
		url:       "http://\x7f", // control byte rejected by URL parser
		formatter: &JSONFormatter{},
		client:    &http.Client{},
	}
	err := s.Send(context.Background(), []Event{{"id": "x"}})
	if err == nil || !strings.Contains(err.Error(), "new request") {
		t.Errorf("expected wrapped new-request err, got: %v", err)
	}
}

// failFormatter is a test-only Formatter that always errors — drives the
// Send-format-error branch deterministically.
type failFormatter struct{}

func (failFormatter) ContentType() string                   { return "application/json" }
func (failFormatter) FormatBatch(_ []Event) ([]byte, error) { return nil, errors.New("bad format") }

// Formatter — close the remaining cefSeverity / syslogSeverity gaps

// TestCEFSeverity_FromTaxonomy drives cefSeverity on the CANONICAL eventTypes
// the classifier actually emits (F-0191). The old table keyed on invented
// prefixes ("iam.", "config.", "proxy.") the classifier never produces, so
// every privilege/kill-switch/node mutation fell through to the lowest default
// severity. These cases pin the taxonomy-derived mapping — and specifically
// that an IAM-policy change scores >= 6 (the finding's named regression).
func TestCEFSeverity_FromTaxonomy(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"auth.login_failure", 7}, // authentication failure
		{"auth.login_success", 5}, // login success (audit)
		{"kill-switch.toggle", 9}, // safety-critical
		{"passthrough.emergency-enable", 9},
		{"iam-policy.create", 6},   // IAM service — privilege
		{"iam-group.delete", 6},    // IAM service
		{"user.update", 6},         // IAM service
		{"node.write-override", 6}, // platform service — config override
		{"settings.update", 6},     // platform service
		{"credential.create", 6},   // secret management (cross-service override)
		{"hook.create", 5},         // compliance config
		{"provider.create", 4},     // gateway data-plane config
		{"traffic.request_blocked", 5},
		{"traffic.rate_limited", 4},
		{"traffic.allowed", 3}, // routine
		{"unknown-resource.frob", 3},
		{"", 3}, // default for empty
	}
	for _, tc := range cases {
		if got := cefSeverity(tc.in); got != tc.want {
			t.Errorf("cefSeverity(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// Named regression guard: a privilege-escalating IAM change must never be
	// exported as low-severity noise.
	if got := cefSeverity("iam-policy.update"); got < 6 {
		t.Errorf("cefSeverity(iam-policy.update) = %d, want >= 6 (F-0191)", got)
	}
}

// TestSyslogSeverity_FromTaxonomy mirrors the CEF test on the syslog scale
// (0 emerg … 7 debug; lower = more severe).
func TestSyslogSeverity_FromTaxonomy(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"auth.login_failure", 4},  // warning
		{"kill-switch.toggle", 2},  // critical
		{"iam-policy.create", 5},   // notice
		{"node.write-override", 5}, // notice
		{"credential.create", 5},   // notice
		{"hook.create", 5},         // notice (compliance)
		{"provider.create", 6},     // info (gateway)
		{"traffic.allowed", 6},     // info
		{"", 6},                    // default info
	}
	for _, tc := range cases {
		if got := syslogSeverity(tc.in); got != tc.want {
			t.Errorf("syslogSeverity(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestSyslogLine_NoTimestamp covers the empty-timestamp fallback (the "-"
// literal) and the empty-hookReason fallback (use eventType as the message).
func TestSyslogLine_NoTimestamp(t *testing.T) {
	evt := Event{"eventType": "session.login", "source": "admin"}
	line := syslogLine(evt)
	if !strings.Contains(line, " - ") {
		t.Errorf("missing '-' fallback timestamp in: %s", line)
	}
	if !strings.Contains(line, "session.login") {
		t.Errorf("missing eventType-as-message in: %s", line)
	}
}

// TestStrField_NonStringValue covers the type-assertion-fail branch (key
// exists but value is not a string).
func TestStrField_NonStringValue(t *testing.T) {
	evt := Event{"count": 42}
	if got := strField(evt, "count"); got != "" {
		t.Errorf("strField on non-string = %q, want empty", got)
	}
	if got := strField(evt, "missing"); got != "" {
		t.Errorf("strField on missing key = %q, want empty", got)
	}
}

// TestJSONFormatter_MarshalError covers the json.Marshal failure branch by
// embedding a channel value (json.Marshal cannot encode channels — surfaces
// json.UnsupportedTypeError).
func TestJSONFormatter_MarshalError(t *testing.T) {
	evt := Event{"badField": make(chan int)}
	_, err := (&JSONFormatter{}).FormatBatch([]Event{evt})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Errorf("expected wrapped marshal err, got: %v", err)
	}
}

// TestFormatBatch_MultiEvent_Newlines covers the `i > 0` separator branch
// in CEFFormatter / SyslogFormatter — single-event batches skip it; the
// multi-event path must insert one '\n' between every pair of lines.
func TestFormatBatch_MultiEvent_Newlines(t *testing.T) {
	events := []Event{
		{"eventType": "iam.update", "sourceIp": "10.0.0.1", "timestamp": "2026-05-17T10:00:00Z"},
		{"eventType": "credential.export", "sourceIp": "10.0.0.2", "timestamp": "2026-05-17T10:00:01Z"},
		{"eventType": "auth.login_failure", "sourceIp": "10.0.0.3", "timestamp": "2026-05-17T10:00:02Z"},
	}

	t.Run("cef", func(t *testing.T) {
		out, err := (&CEFFormatter{}).FormatBatch(events)
		if err != nil {
			t.Fatalf("CEFFormatter: %v", err)
		}
		lines := strings.Split(string(out), "\n")
		if len(lines) != 3 {
			t.Errorf("expected 3 CEF lines, got %d: %s", len(lines), out)
		}
		for i, l := range lines {
			if !strings.HasPrefix(l, "CEF:0|") {
				t.Errorf("line %d not CEF-shaped: %s", i, l)
			}
		}
	})

	t.Run("syslog", func(t *testing.T) {
		out, err := (&SyslogFormatter{}).FormatBatch(events)
		if err != nil {
			t.Fatalf("SyslogFormatter: %v", err)
		}
		lines := strings.Split(string(out), "\n")
		if len(lines) != 3 {
			t.Errorf("expected 3 syslog lines, got %d: %s", len(lines), out)
		}
		for i, l := range lines {
			if !strings.HasPrefix(l, "<") {
				t.Errorf("line %d not syslog-shaped: %s", i, l)
			}
		}
	})
}

// TestFormatBatch_EmptyInput pins the zero-event behaviour for every
// formatter — must not panic and must yield well-formed output for the
// underlying transport.
func TestFormatBatch_EmptyInput(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		b, err := (&JSONFormatter{}).FormatBatch(nil)
		if err != nil {
			t.Errorf("JSONFormatter: %v", err)
		}
		if string(b) != "null" {
			t.Errorf("nil events JSON = %q, want 'null'", b)
		}
	})
	t.Run("cef", func(t *testing.T) {
		b, err := (&CEFFormatter{}).FormatBatch(nil)
		if err != nil {
			t.Errorf("CEFFormatter: %v", err)
		}
		if len(b) != 0 {
			t.Errorf("nil events CEF = %q, want empty", b)
		}
	})
	t.Run("syslog", func(t *testing.T) {
		b, err := (&SyslogFormatter{}).FormatBatch(nil)
		if err != nil {
			t.Errorf("SyslogFormatter: %v", err)
		}
		if len(b) != 0 {
			t.Errorf("nil events syslog = %q, want empty", b)
		}
	})
}

// Concurrency smoke — Poll under contention exercises the mu serialization

// TestPoll_SerializedByMutex_NoRace runs two Polls back-to-back to confirm
// the package compiles cleanly under -race and that the second Poll waits
// for the first (we don't assert ordering — just no race / no panic /
// no deadlock).
func TestPoll_SerializedByMutex_NoRace(t *testing.T) {
	sink := &captureSink{name: "racey"}
	b, mock := newBridgeForTest(t, sink, BridgeConfig{BatchSize: 200, PollInterval: 30 * time.Second})

	// Two complete Poll cycles — each: reload err + 2 checkpoint err + 2
	// queries returning empty + no saves (no events => no Send / no save).
	for range 2 {
		mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
			WithArgs("siem.config").WillReturnError(errors.New("transient"))
		mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
			WithArgs(checkpointKey).WillReturnError(pgx.ErrNoRows)
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(emptyTrafficRows())
		mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
			WithArgs(adminCheckpointKey).WillReturnError(pgx.ErrNoRows)
		mock.ExpectQuery(`FROM "AdminAuditLog"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{
				"id", "timestamp",
				"actorId", "actorLabel", "actorRole",
				"sourceIp", "action", "entityType", "entityId",
				"beforeState", "afterState", "via",
			}))
	}

	var done atomic.Int32
	for range 2 {
		go func() {
			b.Poll(context.Background())
			done.Add(1)
		}()
	}
	// Wait for both to complete deterministically — pgxmock is sequential
	// so they serialize on the bridge mutex.
	deadline := time.After(5 * time.Second)
	for done.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("Poll cycles did not complete: %d/2", done.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}
