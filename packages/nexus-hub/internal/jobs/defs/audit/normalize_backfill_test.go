package audit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// stubNormalizeRegistry is a test double for the NormalizeRegistry seam.
type stubNormalizeRegistry struct {
	payload normcore.NormalizedPayload
	err     error
	calls   int
}

func (s *stubNormalizeRegistry) Normalize(_ context.Context, _ []byte, _ normcore.Meta) (normcore.NormalizedPayload, error) {
	s.calls++
	return s.payload, s.err
}

// silentLogger discards output so test runs don't spam stderr.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newBackfillJob builds a NormalizeBackfill wired against the mock pool and
// stub registry. opsReg is intentionally nil so the counters stay nil — the
// job's nil-guards exercise the no-metrics path that production also uses
// when opsmetrics is disabled.
func newBackfillJob(pool normalizeBackfillQueryer, reg NormalizeRegistry) *NormalizeBackfill {
	return &NormalizeBackfill{
		pool:     pool,
		registry: reg,
		interval: time.Minute,
		logger:   silentLogger(),
	}
}

// inlineBodyEnvelope produces the JSON envelope shape that traffic_event_payload
// stores in inline_request_body / inline_response_body — same shape produced
// by spillstore.EmitBody at the producer side.
func inlineBodyEnvelope(t *testing.T, raw []byte) []byte {
	t.Helper()
	body := sharedaudit.NewInlineBody(raw, int64(len(raw)), false, "application/json")
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

// expectScan queues the SELECT and the returned rows.
func expectScan(mock pgxmock.PgxPoolIface, rows *pgxmock.Rows) {
	mock.ExpectQuery(`FROM traffic_event te`).
		WithArgs(normalizeBackfillBatchSize).
		WillReturnRows(rows)
}

// TestNormalizeBackfill_HappyPath pins the canonical fill path: one row
// scanned, registry returns ok for both directions, UPSERT fires once.
func TestNormalizeBackfill_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rawReq := []byte(`{"model":"gpt-4o","messages":[]}`)
	rawResp := []byte(`{"choices":[]}`)
	rows := pgxmock.NewRows([]string{
		"id", "path", "endpoint_type", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
	}).AddRow(
		"evt-1", "/v1/chat/completions", "chat", "openai", "gpt-4o",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, rawReq), inlineBodyEnvelope(t, rawResp),
	)
	expectScan(mock, rows)

	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).
		WithArgs(
			"evt-1",
			pgxmock.AnyArg(), // request_normalized JSON
			pgxmock.AnyArg(), // response_normalized JSON
			"ok", "ok",
			nil, nil,
			normcore.SchemaVersion,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	reg := &stubNormalizeRegistry{payload: normcore.NormalizedPayload{Kind: "ai-chat"}}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reg.calls != 2 {
		t.Errorf("registry called %d times, want 2 (request + response)", reg.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_RegistryError pins the failed-direction path: the
// registry returns an error for the request side, the row still gets
// upserted with the response side's normalized JSON + status="failed" for
// the request side.
func TestNormalizeBackfill_RegistryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "endpoint_type", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
	}).AddRow(
		"evt-2", "/v1/chat/completions", "chat", "anthropic", "claude",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`malformed`)), inlineBodyEnvelope(t, []byte(`{"ok":true}`)),
	)
	expectScan(mock, rows)

	// UPSERT still fires because at least one direction (response) produced
	// a valid normalized payload. Request lands as request_status='failed'
	// + request_normalized=NULL.
	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).
		WithArgs(
			"evt-2",
			nil,              // request_normalized NULL (failed)
			pgxmock.AnyArg(), // response_normalized JSON
			"failed", "ok",
			"bad json", nil,
			normcore.SchemaVersion,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	reg := &flipFlopRegistry{
		responses: []normResult{
			{err: errors.New("bad json")},
			{payload: normcore.NormalizedPayload{Kind: "ai-chat"}},
		},
	}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_NoCandidates pins the empty-scan path: when no rows
// need backfill, Run returns nil without firing any UPSERT.
func TestNormalizeBackfill_NoCandidates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "endpoint_type", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
	})
	expectScan(mock, rows)

	reg := &stubNormalizeRegistry{}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reg.calls != 0 {
		t.Errorf("registry should not be called when no candidates; got %d", reg.calls)
	}
}

// TestNormalizeBackfill_SpillRefOnlySkipped pins the spill-ref-only skip:
// when both inline bodies are nil (spill-ref-only payload), the row is
// skipped without firing the registry or the UPSERT.
func TestNormalizeBackfill_SpillRefOnlySkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "endpoint_type", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
	}).AddRow(
		"evt-3", "/v1/messages", "chat", "anthropic", "claude",
		(*string)(nil), (*string)(nil),
		nil, nil,
	)
	expectScan(mock, rows)
	// NO UPSERT expected — the row is skipped.

	reg := &stubNormalizeRegistry{}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reg.calls != 0 {
		t.Errorf("registry should not be called for spill-ref-only rows; got %d", reg.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_ScanError pins the SCAN-error surface: a Query
// failure surfaces wrapped to the scheduler.
func TestNormalizeBackfill_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM traffic_event te`).
		WithArgs(normalizeBackfillBatchSize).
		WillReturnError(errors.New("conn refused"))

	job := newBackfillJob(mock, &stubNormalizeRegistry{})
	err := job.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from SCAN failure")
	}
}

// TestNormalizeBackfill_BothDirectionsFailed pins the "no payload produced"
// path: when both directions error, no UPSERT fires (would be all-null,
// pointless to write).
func TestNormalizeBackfill_BothDirectionsFailed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "endpoint_type", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
	}).AddRow(
		"evt-4", "/v1/chat/completions", "chat", "openai", "gpt-4o",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`bad`)), inlineBodyEnvelope(t, []byte(`bad`)),
	)
	expectScan(mock, rows)
	// NO UPSERT expected.

	reg := &stubNormalizeRegistry{err: errors.New("normalize failed")}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reg.calls != 2 {
		t.Errorf("registry called %d times, want 2 (both directions tried)", reg.calls)
	}
}

// TestNormalizeBackfill_ID pins the static job identity (used by the
// scheduler + Hub admin job-list endpoint).
func TestNormalizeBackfill_ID(t *testing.T) {
	job := newBackfillJob(nil, nil)
	if got := job.ID(); got != "normalize-backfill" {
		t.Errorf("ID = %q, want normalize-backfill", got)
	}
	if got := job.Name(); got == "" {
		t.Error("Name must not be empty")
	}
	if got := job.Description(); got == "" {
		t.Error("Description must not be empty")
	}
	if job.RunOnStart() {
		t.Error("RunOnStart must be false (startup grace period)")
	}
	if job.Interval() <= 0 {
		t.Error("Interval must be positive")
	}
}

func ptrStr(s string) *string { return &s }

type normResult struct {
	payload normcore.NormalizedPayload
	err     error
}

// flipFlopRegistry returns a different response on each call, so tests can
// drive each direction (request, then response) with distinct outcomes.
type flipFlopRegistry struct {
	responses []normResult
	idx       int
}

func (f *flipFlopRegistry) Normalize(_ context.Context, _ []byte, _ normcore.Meta) (normcore.NormalizedPayload, error) {
	if f.idx >= len(f.responses) {
		return normcore.NormalizedPayload{}, errors.New("ran out of stub responses")
	}
	r := f.responses[f.idx]
	f.idx++
	return r.payload, r.err
}

// Ensure pgxmock.PgxPoolIface satisfies the seam — compile-time check.
var _ normalizeBackfillQueryer = (interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
})(nil)
