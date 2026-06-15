// Spill-path coverage for normalize_backfill.go: a spilled body is
// fetched from the spill backend and normalized like an inline one; a
// failed fetch records the honest spill_fetch_failed skip reason.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// stubSpillStore serves canned bytes for Get; Put/Delete/Sweep are not
// exercised by the backfill job.
type stubSpillStore struct {
	data   []byte
	getErr error
	gets   int
}

func (s *stubSpillStore) Put(_ context.Context, _ io.Reader, _ int64, _ spillstore.PutOptions) (sharedaudit.SpillRef, error) {
	return sharedaudit.SpillRef{}, errors.New("not used")
}
func (s *stubSpillStore) Get(_ context.Context, _ sharedaudit.SpillRef) (io.ReadCloser, error) {
	s.gets++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
func (s *stubSpillStore) Delete(_ context.Context, _ sharedaudit.SpillRef) error { return nil }
func (s *stubSpillStore) Sweep(_ context.Context, _ time.Time) (int, error)      { return 0, nil }
func (s *stubSpillStore) Backend() string                                        { return "stub" }
func (s *stubSpillStore) Stat(_ context.Context) (spillstore.Stats, error) {
	return spillstore.Stats{}, nil
}

func spillRefJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(sharedaudit.SpillRef{Backend: "s3", Key: "spill/evt/request", Size: 64})
	if err != nil {
		t.Fatalf("marshal ref: %v", err)
	}
	return b
}

// TestNormalizeBackfill_SpillFetchFillsRow pins the spill-aware fill: a
// row with no inline bytes but a request spill ref fetches the object,
// normalizes it, and upserts the sidecar — spilled traffic gets the same
// normalized projection as inline traffic.
func TestNormalizeBackfill_SpillFetchFillsRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-spill", "/v1/chat/completions", "openai", "gpt-4o",
		ptrStr("application/json"), ptrStr("application/json"),
		nil, nil,
		spillRefJSON(t), nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).
		WithArgs(
			"evt-spill",
			pgxmock.AnyArg(), // request_normalized from the fetched bytes
			nil,              // response absent
			"ok", nil,
			nil, nil,
			normcore.SchemaVersion,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	spill := &stubSpillStore{data: []byte(`{"model":"gpt-4o","messages":[]}`)}
	reg := &stubNormalizeRegistry{payload: normcore.NormalizedPayload{Kind: "ai-chat"}}
	job := newBackfillJob(mock, reg)
	job.spill = spill

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spill.gets != 1 {
		t.Errorf("spill.Get called %d times, want 1", spill.gets)
	}
	if reg.calls != 1 {
		t.Errorf("registry called %d times, want 1 (request only)", reg.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_SpillFetchFailureSkipsHonestly pins the failure
// reason: a spill ref whose fetch errors marks the row
// spill_fetch_failed (not the misleading spill_ref_only) so operators
// can tell a missing backend from a missing capability.
func TestNormalizeBackfill_SpillFetchFailureSkipsHonestly(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-spill-err", "/v1/chat/completions", "openai", "gpt-4o",
		(*string)(nil), (*string)(nil),
		nil, nil,
		spillRefJSON(t), nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-spill-err", "spill_fetch_failed", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	job := newBackfillJob(mock, &stubNormalizeRegistry{})
	job.spill = &stubSpillStore{getErr: errors.New("object not found")}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_SpillRefWithoutStoreKeepsLegacyReason pins the
// no-store configuration: a spill-ref-only row with no spill backend
// wired skips as spill_ref_only (the capability, not the backend, is
// what is missing).
func TestNormalizeBackfill_SpillRefWithoutStoreKeepsLegacyReason(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-nostore", "/v1/messages", "anthropic", "claude",
		(*string)(nil), (*string)(nil),
		nil, nil,
		spillRefJSON(t), nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-nostore", "spill_ref_only", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	job := newBackfillJob(mock, &stubNormalizeRegistry{}) // spill stays nil

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_SpillRefDecodeFailureSkips pins the corrupt-ref
// branch: an unparseable spill ref JSON is a fetch failure, not a crash.
func TestNormalizeBackfill_SpillRefDecodeFailureSkips(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-badref", "/v1/messages", "anthropic", "claude",
		(*string)(nil), (*string)(nil),
		nil, nil,
		[]byte(`{corrupt`), nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-badref", "spill_fetch_failed", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	job := newBackfillJob(mock, &stubNormalizeRegistry{})
	job.spill = &stubSpillStore{}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNormalizeBackfill_SpillReadErrorSkips pins the mid-read failure:
// a reader that dies during ReadAll is a fetch failure, recorded as
// spill_fetch_failed.
func TestNormalizeBackfill_SpillReadErrorSkips(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "path", "adapter_type", "model",
		"request_content_type", "response_content_type",
		"inline_request_body", "inline_response_body",
		"request_spill_ref", "response_spill_ref",
	}).AddRow(
		"evt-readerr", "/v1/messages", "anthropic", "claude",
		(*string)(nil), (*string)(nil),
		nil, nil,
		spillRefJSON(t), nil,
	)
	expectScan(mock, rows)
	mock.ExpectExec(`INSERT INTO traffic_event_normalize_skip`).
		WithArgs("evt-readerr", "spill_fetch_failed", normcore.SchemaVersion).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	job := newBackfillJob(mock, &stubNormalizeRegistry{})
	job.spill = &errReadSpillStore{}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// errReadSpillStore returns a reader that fails mid-read.
type errReadSpillStore struct{}

func (s *errReadSpillStore) Put(_ context.Context, _ io.Reader, _ int64, _ spillstore.PutOptions) (sharedaudit.SpillRef, error) {
	return sharedaudit.SpillRef{}, errors.New("not used")
}
func (s *errReadSpillStore) Get(_ context.Context, _ sharedaudit.SpillRef) (io.ReadCloser, error) {
	return io.NopCloser(&failingReader{}), nil
}
func (s *errReadSpillStore) Delete(_ context.Context, _ sharedaudit.SpillRef) error { return nil }
func (s *errReadSpillStore) Sweep(_ context.Context, _ time.Time) (int, error)      { return 0, nil }
func (s *errReadSpillStore) Backend() string                                        { return "stub" }
func (s *errReadSpillStore) Stat(_ context.Context) (spillstore.Stats, error) {
	return spillstore.Stats{}, nil
}

type failingReader struct{}

func (r *failingReader) Read(_ []byte) (int, error) { return 0, errors.New("read aborted") }
