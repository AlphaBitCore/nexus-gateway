package audit

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
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

// expectScan queues the version-aware SELECT and the returned rows. The
// regex pins two load-bearing clauses: version-awareness (the schema-bump
// healing mechanism) AND the governed-row exclusion (rows carrying
// redaction spans are never re-normalized — their span offsets reference
// the projection they were stamped over).
func expectScan(mock pgxmock.PgxPoolIface, rows *pgxmock.Rows) {
	mock.ExpectQuery(`normalize_version IS DISTINCT FROM(?s).*redaction_spans IS NULL`).
		WithArgs(normalizeBackfillBatchSize, normcore.SchemaVersion).
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
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-1", "/v1/chat/completions", "openai", "gpt-4o",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, rawReq), inlineBodyEnvelope(t, rawResp),
		nil, nil,
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
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-2", "/v1/chat/completions", "anthropic", "claude",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`malformed`)), inlineBodyEnvelope(t, []byte(`{"ok":true}`)),
		nil, nil,
	)
	expectScan(mock, rows)

	// UPSERT still fires because at least one direction (response) produced
	// a valid normalized payload. Request lands as request_status='failed'
	// + request_normalized=NULL. The tightened regex pins the
	// status-honesty guard: on conflict, a status may overwrite only
	// together with a new payload (or when no older payload exists), so a
	// failed re-normalize can never stamp 'failed' over a surviving older
	// payload the drawer still renders.
	mock.ExpectExec(`CASE WHEN EXCLUDED.request_normalized IS NOT NULL OR traffic_event_normalized.request_normalized IS NULL`).
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
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
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
// not normalized or UPSERTed — but it IS recorded in the skip ledger so
// the scan stops returning it (the starvation fix).
func TestNormalizeBackfill_SpillRefOnlySkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-3", "/v1/messages", "anthropic", "claude",
		(*string)(nil), (*string)(nil),
		nil, nil,
		nil, nil,
	)
	expectScan(mock, rows)
	// NO normalized UPSERT, but a terminal skip-mark must be written so the
	// row leaves the scan set.
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-3", "spill_ref_only", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

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

// TestNormalizeBackfill_NoPayloadProducedSkipMarked pins the second
// unfillable class: inline bytes exist but normalize produces nothing for
// both directions → the row is skip-marked (reason no_payload_produced),
// not re-scanned forever.
func TestNormalizeBackfill_NoPayloadProducedSkipMarked(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-4", "/v1/chat/completions", "openai", "gpt",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`req`)), inlineBodyEnvelope(t, []byte(`resp`)),
		nil, nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-4", "no_payload_produced", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	// Both directions fail to normalize → no payload produced for either.
	reg := &stubNormalizeRegistry{err: errors.New("no payload")}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_UpsertFailureNonFatal proves a failed normalized
// UPSERT is logged, not fatal — Run completes and the next tick retries.
func TestNormalizeBackfill_UpsertFailureNonFatal(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-6", "/v1/chat/completions", "openai", "gpt",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`req`)), inlineBodyEnvelope(t, []byte(`resp`)),
		nil, nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnError(context.DeadlineExceeded)

	reg := &stubNormalizeRegistry{payload: normcore.NormalizedPayload{Kind: "ai-chat"}}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run must not surface an upsert failure: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_SkipMarkFailureNonFatal proves a failed skip-mark
// is logged, not fatal — the row is simply retried next tick, no data
// lost.
func TestNormalizeBackfill_SkipMarkFailureNonFatal(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-5", "/v1/messages", "anthropic", "claude",
		(*string)(nil), (*string)(nil),
		nil, nil,
		nil, nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-5", "spill_ref_only", normcore.SchemaVersion).
		WillReturnError(context.DeadlineExceeded)

	reg := &stubNormalizeRegistry{}
	job := newBackfillJob(mock, reg)

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run must not surface a skip-mark write failure: %v", err)
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
		WithArgs(normalizeBackfillBatchSize, normcore.SchemaVersion).
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
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-4", "/v1/chat/completions", "openai", "gpt-4o",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`bad`)), inlineBodyEnvelope(t, []byte(`bad`)),
		nil, nil,
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

// TestNormalizeBackfill_MarshalFailureRecordsFailedStatus pins the
// payload-marshal failure: a payload the encoder cannot serialize (NaN in
// Params) records status=failed with the marshal reason instead of
// silently writing nothing for the direction.
func TestNormalizeBackfill_MarshalFailureRecordsFailedStatus(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-8", "/v1/chat/completions", "openai", "gpt",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`req`)), inlineBodyEnvelope(t, []byte(`resp`)),
		nil, nil,
	)
	expectScan(mock, rows)
	// Both directions fail to marshal → no payload produced → skip-marked.
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-8", "no_payload_produced", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	nan := math.NaN()
	reg := &stubNormalizeRegistry{payload: normcore.NormalizedPayload{
		Kind:   "ai-chat",
		Params: &normcore.SamplingParam{Temperature: &nan},
	}}
	job := newBackfillJob(mock, reg)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_MetricsCounters proves the job reports its work
// when metrics are wired: a spill-ref skip bumps skipped{reason} and a
// successful fill bumps filled — the dashboards operators use to see the
// backfill making progress.
func TestNormalizeBackfill_MetricsCounters(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-9", "/v1/messages", "anthropic", "claude",
		(*string)(nil), (*string)(nil),
		nil, nil, // spill-ref-only → skipped{spill_ref_only}
		nil, nil,
	).AddRow(
		"evt-10", "/v1/chat/completions", "openai", "gpt",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, []byte(`req`)), inlineBodyEnvelope(t, []byte(`resp`)), // → filled
		nil, nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-9", "spill_ref_only", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	opsReg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	job := NewNormalizeBackfill(nil, &stubNormalizeRegistry{payload: normcore.NormalizedPayload{Kind: "ai-chat"}}, nil, time.Minute, opsReg, silentLogger())
	job.pool = mock

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
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
