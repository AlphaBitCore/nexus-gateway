package asyncjob

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func newMock(t *testing.T) (pgxmock.PgxPoolIface, *DB) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet pgxmock expectations: %v", err)
		}
		mock.Close()
	})
	return mock, New(mock)
}

var (
	// insertRe pins the FULL insert shape end-anchored at the VALUES list:
	// an ON CONFLICT suffix (the F-sec-1 upsert hijack the store must never
	// grow) would fail the match, so "never upserts" is a real pin, not a
	// substring hit.
	insertRe = regexp.MustCompile(`(?s)INSERT INTO gateway_async_job \(\s*provider_id, id, endpoint_type, virtual_key_id, organization_id,\s*project_id, credential_id, model_id, requested_units, status,\s*submit_trace_id, expires_at\s*\) VALUES \(\$1,\$2,\$3,\$4,\$5,\$6,\$7,\$8,\$9,\$10,\$11,\$12\)\s*$`)
	selectRe = regexp.MustCompile(`SELECT provider_id, .* FROM gateway_async_job WHERE provider_id = \$1 AND id = \$2`)
)

func sampleJob() Job {
	return Job{
		ProviderID:     "prov-1",
		ID:             "video_abc",
		EndpointType:   "video_generation",
		VirtualKeyID:   "vk-uuid-1",
		OrganizationID: "org-1",
		ProjectID:      "proj-1",
		CredentialID:   "cred-1",
		ModelID:        "sora-2",
		RequestedUnits: 8,
		Status:         StatusQueued,
		SubmitTraceID:  "trace-1",
	}
}

func TestInsert_BindsAllColumns(t *testing.T) {
	mock, db := newMock(t)
	j := sampleJob()
	mock.ExpectExec(insertRe.String()).
		WithArgs(j.ProviderID, j.ID, j.EndpointType, j.VirtualKeyID, j.OrganizationID,
			j.ProjectID, j.CredentialID, j.ModelID, j.RequestedUnits, j.Status,
			j.SubmitTraceID, j.ExpiresAt).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := db.Insert(context.Background(), j); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func TestInsert_EmptyOptionalsBindNULL(t *testing.T) {
	// project_id / credential_id are nullable columns; the empty string must
	// bind SQL NULL, not '' (a '' would break the COALESCE-read contract and
	// look like a real project id downstream).
	mock, db := newMock(t)
	j := sampleJob()
	j.ProjectID, j.CredentialID = "", ""
	mock.ExpectExec(insertRe.String()).
		WithArgs(j.ProviderID, j.ID, j.EndpointType, j.VirtualKeyID, j.OrganizationID,
			nil, nil, j.ModelID, j.RequestedUnits, j.Status,
			j.SubmitTraceID, j.ExpiresAt).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := db.Insert(context.Background(), j); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// The hijack guard: a duplicate (provider_id, id) MUST surface ErrConflict —
// plain INSERT semantics, never an upsert that would rebind virtual_key_id to
// a second submitter.
func TestInsert_ConflictFailsNeverUpserts(t *testing.T) {
	mock, db := newMock(t)
	j := sampleJob()
	mock.ExpectExec(insertRe.String()).
		WithArgs(j.ProviderID, j.ID, j.EndpointType, j.VirtualKeyID, j.OrganizationID,
			j.ProjectID, j.CredentialID, j.ModelID, j.RequestedUnits, j.Status,
			j.SubmitTraceID, j.ExpiresAt).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "gateway_async_job_pkey"})
	err := db.Insert(context.Background(), j)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v want ErrConflict", err)
	}
}

func TestInsert_OtherDBErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	j := sampleJob()
	boom := errors.New("connection refused")
	mock.ExpectExec(insertRe.String()).
		WithArgs(j.ProviderID, j.ID, j.EndpointType, j.VirtualKeyID, j.OrganizationID,
			j.ProjectID, j.CredentialID, j.ModelID, j.RequestedUnits, j.Status,
			j.SubmitTraceID, j.ExpiresAt).
		WillReturnError(boom)
	err := db.Insert(context.Background(), j)
	if err == nil || errors.Is(err, ErrConflict) || !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped non-conflict error", err)
	}
}

// A unique violation from anything OTHER than the composite PK (a future
// unique index) must NOT masquerade as the hijack-guard conflict — and a
// non-unique-violation PgError likewise stays a wrapped error.
func TestInsert_ForeignPgErrorsAreNotConflict(t *testing.T) {
	for _, pgErr := range []*pgconn.PgError{
		{Code: "23505", ConstraintName: "some_future_unique_idx"},
		{Code: "23503", ConstraintName: "some_fk"}, // FK violation
	} {
		mock, db := newMock(t)
		j := sampleJob()
		mock.ExpectExec(insertRe.String()).
			WithArgs(j.ProviderID, j.ID, j.EndpointType, j.VirtualKeyID, j.OrganizationID,
				j.ProjectID, j.CredentialID, j.ModelID, j.RequestedUnits, j.Status,
				j.SubmitTraceID, j.ExpiresAt).
			WillReturnError(pgErr)
		err := db.Insert(context.Background(), j)
		if errors.Is(err, ErrConflict) {
			t.Errorf("PgError %s/%s mapped to ErrConflict; only the composite-PK violation may", pgErr.Code, pgErr.ConstraintName)
		}
		if err == nil || !errors.As(err, new(*pgconn.PgError)) {
			t.Errorf("PgError %s/%s: err = %v want the wrapped PgError", pgErr.Code, pgErr.ConstraintName, err)
		}
	}
}

// Write-side vocabulary discipline: an unmapped provider state fails loud at
// the single choke point instead of corrupting the lifecycle state machine.
func TestWrites_RejectOutOfVocabularyStatus(t *testing.T) {
	_, db := newMock(t) // no expectations: the guard must fire before any SQL
	j := sampleJob()
	j.Status = "processing" // a plausible unmapped provider state
	if err := db.Insert(context.Background(), j); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("Insert err = %v want ErrInvalidStatus", err)
	}
	err := db.MarkObserved(context.Background(), "prov-1", "video_abc", Observation{Status: "PROCESSING"})
	if !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("MarkObserved err = %v want ErrInvalidStatus", err)
	}
}

func jobRows(j Job, created time.Time) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"provider_id", "id", "endpoint_type", "virtual_key_id", "organization_id",
		"project_id", "credential_id", "model_id", "requested_units",
		"status", "cost_reconciled", "submit_trace_id", "created_at", "completed_at", "canceled_at", "expires_at",
	}).AddRow(
		j.ProviderID, j.ID, j.EndpointType, j.VirtualKeyID, j.OrganizationID,
		j.ProjectID, j.CredentialID, j.ModelID, j.RequestedUnits,
		j.Status, j.CostReconciled, j.SubmitTraceID, created, j.CompletedAt, j.CanceledAt, j.ExpiresAt,
	)
}

func TestGetByID_RoundTrip(t *testing.T) {
	mock, db := newMock(t)
	want := sampleJob()
	want.Status = StatusInProgress
	created := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	exp := created.Add(48 * time.Hour)
	want.ExpiresAt = &exp

	mock.ExpectQuery(selectRe.String()).
		WithArgs("prov-1", "video_abc").
		WillReturnRows(jobRows(want, created))

	got, err := db.GetByID(context.Background(), "prov-1", "video_abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want.CreatedAt = created
	if got.ProviderID != want.ProviderID || got.ID != want.ID ||
		got.VirtualKeyID != want.VirtualKeyID || got.CredentialID != want.CredentialID ||
		got.Status != StatusInProgress || got.RequestedUnits != 8 ||
		!got.CreatedAt.Equal(created) || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// The 404-non-disclosure contract rests on this sentinel.
func TestGetByID_NotFound(t *testing.T) {
	mock, db := newMock(t)
	mock.ExpectQuery(selectRe.String()).
		WithArgs("prov-1", "nope").
		WillReturnError(pgx.ErrNoRows)
	_, err := db.GetByID(context.Background(), "prov-1", "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v want ErrNotFound", err)
	}
}

func TestGetByID_QueryErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("io timeout")
	mock.ExpectQuery(selectRe.String()).
		WithArgs("prov-1", "video_abc").
		WillReturnError(boom)
	_, err := db.GetByID(context.Background(), "prov-1", "video_abc")
	if err == nil || errors.Is(err, ErrNotFound) || !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped non-notfound error", err)
	}
}

// ownedRe pins the owned-lookup shape: the virtual_key_id predicate IS the
// authz binding (a foreign VK's job matches zero rows → the same ErrNotFound
// as a missing job — the 404 non-disclosure cannot leak existence), the
// endpoint_type predicate keeps a future batch job invisible to the video
// surface, and LIMIT 2 caps the scan at collision-proof.
var ownedRe = regexp.MustCompile(`(?s)SELECT provider_id, .* FROM gateway_async_job\s*WHERE id = \$1 AND virtual_key_id = \$2 AND endpoint_type = \$3 LIMIT 2`)

func TestGetOwned_RoundTrip(t *testing.T) {
	mock, db := newMock(t)
	want := sampleJob()
	want.Status = StatusInProgress
	created := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(ownedRe.String()).
		WithArgs("video_abc", "vk-uuid-1", "video_generation").
		WillReturnRows(jobRows(want, created))

	got, err := db.GetOwned(context.Background(), "video_abc", "vk-uuid-1", "video_generation")
	if err != nil {
		t.Fatalf("get owned: %v", err)
	}
	if got.ProviderID != "prov-1" || got.ID != "video_abc" || got.CredentialID != "cred-1" ||
		got.Status != StatusInProgress || got.RequestedUnits != 8 {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

// Zero rows — a missing job AND a foreign VK's job are indistinguishable by
// construction (the VK predicate lives in the query): both are ErrNotFound.
func TestGetOwned_NotFoundAndForeignVKCollapse(t *testing.T) {
	mock, db := newMock(t)
	mock.ExpectQuery(ownedRe.String()).
		WithArgs("video_abc", "vk-other", "video_generation").
		WillReturnRows(pgxmock.NewRows([]string{"provider_id"}))
	_, err := db.GetOwned(context.Background(), "video_abc", "vk-other", "video_generation")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v want ErrNotFound", err)
	}
}

// Two providers issued the same id string to this VK: the store must refuse
// to guess (a silent pick could relay the poll/delete to the wrong provider
// account) — ErrAmbiguous, fail loud.
func TestGetOwned_TwoProvidersSameID_Ambiguous(t *testing.T) {
	mock, db := newMock(t)
	created := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	a := sampleJob()
	b := sampleJob()
	b.ProviderID = "prov-2"
	rows := jobRows(a, created).AddRow(
		b.ProviderID, b.ID, b.EndpointType, b.VirtualKeyID, b.OrganizationID,
		b.ProjectID, b.CredentialID, b.ModelID, b.RequestedUnits,
		b.Status, b.CostReconciled, b.SubmitTraceID, created, b.CompletedAt, b.CanceledAt, b.ExpiresAt,
	)
	mock.ExpectQuery(ownedRe.String()).
		WithArgs("video_abc", "vk-uuid-1", "video_generation").
		WillReturnRows(rows)
	_, err := db.GetOwned(context.Background(), "video_abc", "vk-uuid-1", "video_generation")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v want ErrAmbiguous", err)
	}
}

// A malformed row (type drift between the SELECT list and the scan) must
// surface as a wrapped error, never a zero-value Job a handler would then
// relay against the wrong provider scope.
func TestGetOwned_ScanErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	rows := pgxmock.NewRows([]string{
		"provider_id", "id", "endpoint_type", "virtual_key_id", "organization_id",
		"project_id", "credential_id", "model_id", "requested_units",
		"status", "cost_reconciled", "submit_trace_id", "created_at", "completed_at", "canceled_at", "expires_at",
	}).AddRow(
		"prov-1", "video_abc", "video_generation", "vk-uuid-1", "org-1",
		"proj-1", "cred-1", "sora-2", 8.0,
		StatusQueued, "not-a-bool", "trace-1", time.Now(), nil, nil, nil,
	)
	mock.ExpectQuery(ownedRe.String()).
		WithArgs("video_abc", "vk-uuid-1", "video_generation").
		WillReturnRows(rows)
	_, err := db.GetOwned(context.Background(), "video_abc", "vk-uuid-1", "video_generation")
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v want wrapped scan error", err)
	}
}

// A mid-iteration rows error (connection dropped between rows) must surface,
// not silently truncate to "one row found".
func TestGetOwned_RowsErrWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("conn reset mid-rows")
	created := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	rows := jobRows(sampleJob(), created).CloseError(boom)
	mock.ExpectQuery(ownedRe.String()).
		WithArgs("video_abc", "vk-uuid-1", "video_generation").
		WillReturnRows(rows)
	_, err := db.GetOwned(context.Background(), "video_abc", "vk-uuid-1", "video_generation")
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrAmbiguous) || !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped rows error", err)
	}
}

func TestGetOwned_QueryErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("io timeout")
	mock.ExpectQuery(ownedRe.String()).
		WithArgs("video_abc", "vk-uuid-1", "video_generation").
		WillReturnError(boom)
	_, err := db.GetOwned(context.Background(), "video_abc", "vk-uuid-1", "video_generation")
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrAmbiguous) || !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped non-sentinel error", err)
	}
}

func TestMarkObserved_UpdatesLastObservedCache(t *testing.T) {
	mock, db := newMock(t)
	done := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	exp := done.Add(time.Hour)
	mock.ExpectExec(`UPDATE gateway_async_job\s+SET status = \$3`).
		WithArgs("prov-1", "video_abc", StatusCompleted, &done, &exp).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	err := db.MarkObserved(context.Background(), "prov-1", "video_abc",
		Observation{Status: StatusCompleted, CompletedAt: &done, ExpiresAt: &exp})
	if err != nil {
		t.Fatalf("mark observed: %v", err)
	}
}

func TestMarkObserved_MissingRowIsNotFound(t *testing.T) {
	mock, db := newMock(t)
	mock.ExpectExec(`UPDATE gateway_async_job\s+SET status = \$3`).
		WithArgs("prov-1", "gone", StatusInProgress, (*time.Time)(nil), (*time.Time)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	err := db.MarkObserved(context.Background(), "prov-1", "gone", Observation{Status: StatusInProgress})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v want ErrNotFound", err)
	}
}

func TestMarkObserved_ExecErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("deadlock")
	mock.ExpectExec(`UPDATE gateway_async_job\s+SET status = \$3`).
		WithArgs("prov-1", "video_abc", StatusFailed, (*time.Time)(nil), (*time.Time)(nil)).
		WillReturnError(boom)
	err := db.MarkObserved(context.Background(), "prov-1", "video_abc", Observation{Status: StatusFailed})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped exec error", err)
	}
}

// The single-cost-row invariant rests on this: exactly ONE caller wins the
// claim (RowsAffected=1 under the cost_reconciled=false predicate); a second
// caller matches zero rows and must get false with no error.
func TestReconcileOnce_FirstClaimWinsSecondLoses(t *testing.T) {
	mock, db := newMock(t)
	reconcileSQL := `UPDATE gateway_async_job\s+SET cost_reconciled = true\s+WHERE provider_id = \$1 AND id = \$2 AND cost_reconciled = false`
	mock.ExpectExec(reconcileSQL).
		WithArgs("prov-1", "video_abc").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(reconcileSQL).
		WithArgs("prov-1", "video_abc").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	first, err := db.ReconcileOnce(context.Background(), "prov-1", "video_abc")
	if err != nil || !first {
		t.Fatalf("first claim = (%v, %v), want (true, nil)", first, err)
	}
	second, err := db.ReconcileOnce(context.Background(), "prov-1", "video_abc")
	if err != nil || second {
		t.Fatalf("second claim = (%v, %v), want (false, nil)", second, err)
	}
}

func TestReconcileOnce_ExecErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("conn closed")
	mock.ExpectExec(`UPDATE gateway_async_job\s+SET cost_reconciled = true`).
		WithArgs("prov-1", "video_abc").
		WillReturnError(boom)
	claimed, err := db.ReconcileOnce(context.Background(), "prov-1", "video_abc")
	if claimed || !errors.Is(err, boom) {
		t.Fatalf("= (%v, %v), want (false, wrapped error)", claimed, err)
	}
}

// markCanceledRe pins the VK defense-in-depth predicate: dropping
// `AND virtual_key_id = $4` (which would re-open cross-VK cancel on a
// forgotten handler authz check) fails the match.
// markCanceledRe pins the terminal-status guard: a delete after completion
// must keep `completed` (the record that the render finished and was
// accounted) while still stamping canceled_at.
const markCanceledRe = `UPDATE gateway_async_job\s+SET status = CASE WHEN status IN \(\$5, \$6, \$7\) THEN status ELSE \$3 END,\s+canceled_at = now\(\)\s+WHERE provider_id = \$1 AND id = \$2 AND virtual_key_id = \$4`

func TestMarkCanceled(t *testing.T) {
	mock, db := newMock(t)
	mock.ExpectExec(markCanceledRe).
		WithArgs("prov-1", "video_abc", StatusCanceled, "vk-uuid-1",
			StatusCompleted, StatusFailed, StatusExpired).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := db.MarkCanceled(context.Background(), "prov-1", "video_abc", "vk-uuid-1"); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
}

// A missing row AND a foreign VK's row are indistinguishable to the caller —
// both zero rows → ErrNotFound, the 404 non-disclosure.
func TestMarkCanceled_MissingOrForeignVKIsNotFound(t *testing.T) {
	mock, db := newMock(t)
	mock.ExpectExec(markCanceledRe).
		WithArgs("prov-1", "video_abc", StatusCanceled, "vk-OTHER",
			StatusCompleted, StatusFailed, StatusExpired).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	if err := db.MarkCanceled(context.Background(), "prov-1", "video_abc", "vk-OTHER"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v want ErrNotFound", err)
	}
}

func TestMarkCanceled_ExecErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("tcp reset")
	mock.ExpectExec(markCanceledRe).
		WithArgs("prov-1", "video_abc", StatusCanceled, "vk-uuid-1",
			StatusCompleted, StatusFailed, StatusExpired).
		WillReturnError(boom)
	if err := db.MarkCanceled(context.Background(), "prov-1", "video_abc", "vk-uuid-1"); !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped exec error", err)
	}
}

// The render bound (SDD D-V9): submit admission counts the VK's rows NOT in
// a terminal status — the NOT IN formulation means an out-of-vocabulary
// status counts AGAINST the cap (fail toward bounding spend) rather than
// silently freeing a render slot.
func TestCountNonTerminal(t *testing.T) {
	mock, db := newMock(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM gateway_async_job\s+WHERE virtual_key_id = \$1 AND status NOT IN \(\$2, \$3, \$4, \$5\)`).
		WithArgs("vk-uuid-1", StatusCompleted, StatusFailed, StatusCanceled, StatusExpired).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(3))
	n, err := db.CountNonTerminal(context.Background(), "vk-uuid-1")
	if err != nil || n != 3 {
		t.Fatalf("= (%d, %v), want (3, nil)", n, err)
	}
}

func TestCountNonTerminal_QueryErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("timeout")
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM gateway_async_job`).
		WithArgs("vk-uuid-1", StatusCompleted, StatusFailed, StatusCanceled, StatusExpired).
		WillReturnError(boom)
	if _, err := db.CountNonTerminal(context.Background(), "vk-uuid-1"); !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped query error", err)
	}
}

// markExpiredRe pins BOTH status arms with their cutoff pairing: the terminal
// IN-list judged by $5 (the 30d cutoff) and the NOT IN complement judged by
// $6 (the 7d cutoff). Swapping the cutoffs inside the SQL fails the match —
// the arm↔cutoff pairing is the retention contract, not just arg order.
const markExpiredRe = `(?s)UPDATE gateway_async_job\s+SET status = \$1\s+WHERE status <> \$1\s+AND \(\s+\(status IN \(\$2, \$3, \$4\) AND created_at < \$5\)\s+OR \(status NOT IN \(\$2, \$3, \$4\) AND created_at < \$6\)\s+\)`

// Retention marks, never deletes; already-expired rows are excluded
// (idempotent sweep) and an out-of-vocabulary status falls into the NOT IN
// arm (swept at the shorter cutoff instead of living forever).
func TestMarkExpired_BindsCutoffsPerStatusArm(t *testing.T) {
	mock, db := newMock(t)
	termCut := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)   // terminal > 30d
	nontermCut := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC) // non-terminal > 7d
	mock.ExpectExec(markExpiredRe).
		WithArgs(StatusExpired,
			StatusCompleted, StatusFailed, StatusCanceled, termCut, nontermCut).
		WillReturnResult(pgxmock.NewResult("UPDATE", 5))
	n, err := db.MarkExpired(context.Background(), termCut, nontermCut)
	if err != nil || n != 5 {
		t.Fatalf("= (%d, %v), want (5, nil)", n, err)
	}
}

func TestMarkExpired_ExecErrorWrapped(t *testing.T) {
	mock, db := newMock(t)
	boom := errors.New("permission denied")
	mock.ExpectExec(markExpiredRe).
		WithArgs(StatusExpired,
			StatusCompleted, StatusFailed, StatusCanceled, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(boom)
	if _, err := db.MarkExpired(context.Background(), time.Now(), time.Now()); !errors.Is(err, boom) {
		t.Fatalf("err = %v want wrapped exec error", err)
	}
}

func TestTerminal(t *testing.T) {
	for s, want := range map[string]bool{
		StatusQueued:     false,
		StatusInProgress: false,
		StatusCompleted:  true,
		StatusFailed:     true,
		StatusCanceled:   true,
		StatusExpired:    true,
		"bogus":          false,
	} {
		if got := Terminal(s); got != want {
			t.Errorf("Terminal(%q) = %v want %v", s, got, want)
		}
	}
}

// DB must satisfy the Store interface handlers are tested against.
var _ Store = (*DB)(nil)
