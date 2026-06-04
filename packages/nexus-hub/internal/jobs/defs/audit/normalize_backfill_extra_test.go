// Extra coverage for normalize_backfill.go: constructor metric wiring,
// upsert error fallback, extractInlineBytes edge cases, and the
// bumpErr/bumpSkipped paths exercised when opsmetrics counters are wired.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// TestNewNormalizeBackfill_DefaultIntervalAndCounters pins the constructor's
// metric registration path: with a non-nil opsmetrics.Registry, all four
// counters are wired; interval <=0 falls back to the 5-minute default.
//
// Passes a nil *pgxpool.Pool because the constructor only stores the pool;
// no method call happens until Run, which the test does not invoke.
func TestNewNormalizeBackfill_DefaultIntervalAndCounters(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	job := NewNormalizeBackfill(nil, &stubNormalizeRegistry{}, 0, reg, silentLogger())
	if job.interval != 5*time.Minute {
		t.Errorf("default interval = %v, want 5m", job.interval)
	}
	if job.scanned == nil {
		t.Error("scanned counter not wired with non-nil opsReg")
	}
	if job.filled == nil {
		t.Error("filled counter not wired")
	}
	if job.skipped == nil {
		t.Error("skipped counter not wired")
	}
	if job.errors == nil {
		t.Error("errors counter not wired")
	}
}

// TestNewNormalizeBackfill_NilRegistry confirms the no-metrics path: nil
// opsReg leaves all counter fields nil so the runtime's nil-guards take
// over (matches the pattern in audit_freshness_check.go).
func TestNewNormalizeBackfill_NilRegistry(t *testing.T) {
	job := NewNormalizeBackfill(nil, &stubNormalizeRegistry{}, 2*time.Minute, nil, silentLogger())
	if job.scanned != nil || job.filled != nil || job.skipped != nil || job.errors != nil {
		t.Error("counters should stay nil when opsReg is nil")
	}
	if job.interval != 2*time.Minute {
		t.Errorf("interval = %v, want 2m (caller-supplied)", job.interval)
	}
}

// TestNormalizeBackfill_UpsertExecError pins the UPSERT-failure branch:
// when the registry produces a payload but the INSERT errors, the row is
// logged + the errors counter bumps, without panicking.
func TestNormalizeBackfill_UpsertExecError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rawReq := []byte(`{"model":"gpt-4o"}`)
	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
	}).AddRow(
		"evt-err", "/v1/chat/completions", "openai", "gpt-4o",
		ptrStr("application/json"), ptrStr("application/json"),
		inlineBodyEnvelope(t, rawReq), nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnError(errors.New("23505 duplicate key"))

	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	job := NewNormalizeBackfill(nil, &stubNormalizeRegistry{}, 5*time.Minute, reg, silentLogger())
	// Swap in mock pool for the call. The constructor stored nil; replace
	// directly via the field since this is white-box testing.
	job.pool = mock

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestExtractInlineBytes_MalformedEnvelope pins the JSON-decode error
// fallback: a corrupted envelope returns nil bytes (caller treats as
// "no inline bytes").
func TestExtractInlineBytes_MalformedEnvelope(t *testing.T) {
	got := extractInlineBytes([]byte(`not json`))
	if got != nil {
		t.Errorf("malformed envelope should return nil, got %q", got)
	}
}

// TestExtractInlineBytes_EmptyInput pins the early-return for nil / empty
// envelope (a column NULL in the DB → nil byte slice).
func TestExtractInlineBytes_EmptyInput(t *testing.T) {
	if got := extractInlineBytes(nil); got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
	if got := extractInlineBytes([]byte{}); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
}

// TestExtractInlineBytes_SpillRefBody pins the kind-mismatch branch: a body
// envelope whose Kind != BodyInline returns nil (spill-ref / absent bodies
// have no inline bytes).
func TestExtractInlineBytes_SpillRefBody(t *testing.T) {
	body := sharedaudit.Body{
		Kind:      sharedaudit.BodyAbsent,
		SizeBytes: 100,
	}
	envelope, _ := json.Marshal(body)
	got := extractInlineBytes(envelope)
	if got != nil {
		t.Errorf("spill-ref body should return nil inline bytes, got %v", got)
	}
}

// TestExtractInlineBytes_InlineHappy pins the success path: a well-formed
// BodyInline envelope round-trips InlineBytes back to the caller.
func TestExtractInlineBytes_InlineHappy(t *testing.T) {
	want := []byte(`{"prompt":"hello"}`)
	body := sharedaudit.NewInlineBody(want, int64(len(want)), false, "application/json")
	envelope, _ := json.Marshal(body)

	got := extractInlineBytes(envelope)
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNilJSONIfEmptyAndNilIfEmpty pins the two small SQL-NULL helpers.
func TestNilJSONIfEmptyAndNilIfEmpty(t *testing.T) {
	if got := nilJSONIfEmpty(nil); got != nil {
		t.Errorf("nilJSONIfEmpty(nil) should be nil, got %v", got)
	}
	if got := nilJSONIfEmpty([]byte{}); got != nil {
		t.Errorf("nilJSONIfEmpty([]) should be nil, got %v", got)
	}
	if got := nilJSONIfEmpty([]byte(`{}`)); got == nil {
		t.Error("nilJSONIfEmpty(non-empty) should be non-nil")
	}
	if got := nilIfEmpty(""); got != nil {
		t.Errorf("nilIfEmpty(\"\") should be nil, got %v", got)
	}
	if got := nilIfEmpty("x"); got == nil {
		t.Error("nilIfEmpty(non-empty) should be non-nil")
	}
}

// TestNormalizeBackfill_ScanRowError pins the inner Scan-error branch
// (different from the outer Query error covered elsewhere). The row's
// scan target type-mismatches against a returned NUL — Scan rejects.
func TestNormalizeBackfill_ScanRowError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Return rows whose column count doesn't match Scan's destination shape
	// (9 destinations expected; only 1 column supplied) — Scan must error.
	rows := pgxmock.NewRows([]string{"only_one"}).AddRow("oops")
	mock.ExpectQuery(`FROM traffic_event te`).
		WithArgs(normalizeBackfillBatchSize).
		WillReturnRows(rows)

	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	job := NewNormalizeBackfill(nil, &stubNormalizeRegistry{}, 5*time.Minute, reg, silentLogger())
	job.pool = mock

	err := job.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from scan-row failure")
	}
}
