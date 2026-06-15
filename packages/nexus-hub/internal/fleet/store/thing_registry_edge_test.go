package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// TestGetThing_NullJSONBColumnsYieldEmptyState pins decodeJSONB's
// NULL/'null' tolerance: a freshly enrolled Thing whose desired /
// reported / metadata / reported_outcomes columns are still NULL (or a
// literal SQL 'null') must come back as a valid Thing with empty maps,
// not error out — NULL JSONB is a legitimate pre-first-push state.
func TestGetThing_NullJSONBColumnsYieldEmptyState(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t\s+LEFT JOIN "DeviceAssignment"`).
		WithArgs("thing-1").
		WillReturnRows(pgxmock.NewRows(getThingCols).AddRow(
			"thing-1", "agent", "host", "1.0", "addr",
			"sso", "bearer", "http",
			"enrolled",
			[]byte(nil), []byte("null"), // desired NULL, reported 'null'
			int64(0), int64(0),
			[]byte(nil), &now, now, // metadata NULL
			[]byte(nil), (*time.Time)(nil), // reported_outcomes NULL
			"", "", "", "", "",
			"", "", "", "",
		))
	got, err := New(mock).GetThing(context.Background(), "thing-1")
	if err != nil {
		t.Fatalf("GetThing with NULL JSONB columns must succeed: %v", err)
	}
	if len(got.Desired) != 0 || len(got.Reported) != 0 ||
		len(got.Metadata) != 0 || len(got.ReportedOutcomes) != 0 {
		t.Errorf("NULL columns must decode to empty maps; got desired=%v reported=%v metadata=%v outcomes=%v",
			got.Desired, got.Reported, got.Metadata, got.ReportedOutcomes)
	}
}

// TestUpsertThingEnrollment_UnmarshalableMetadataDegradesToEmpty pins
// the metadata-marshal fallback: enrollment must never fail because a
// caller passed an unmarshalable metadata value — the row is written
// with an empty '{}' metadata object instead, so the Thing still
// enrolls and later heartbeats can repopulate metadata.
func TestUpsertThingEnrollment_UnmarshalableMetadataDegradesToEmpty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`INSERT INTO thing\b`).
		WithArgs(
			"thing-1", "agent", "host", "1.0", "addr",
			"sso", "bearer", "http", "online",
			[]byte("{}"), // metadata degraded to empty object
			[]byte("{}"), // nil desired
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	err := New(mock).UpsertThingEnrollment(context.Background(), UpsertThingParams{
		ID: "thing-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
		EnrolledBy: "sso", Status: "online",
		Metadata: map[string]any{"bad": make(chan int)},
	})
	if err != nil {
		t.Fatalf("enrollment must survive unmarshalable metadata: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("metadata must be written as '{}': %v", err)
	}
}

// TestUpsertThingEnrollmentWithDesiredVer_UnmarshalableMetadataDegradesToEmpty
// pins the same metadata fallback on the desired_ver-stamping variant:
// the row lands with '{}' metadata, the supplied desired_ver, and a
// NULL physical_id when none was given.
func TestUpsertThingEnrollmentWithDesiredVer_UnmarshalableMetadataDegradesToEmpty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`INSERT INTO thing\b`).
		WithArgs(
			"thing-1", "agent", "host", "1.0", "addr",
			"sso", "bearer", "http", "online",
			[]byte("{}"), // metadata degraded to empty object
			[]byte("{}"), // nil desired
			int64(5),     // desiredVer
			nil,          // empty PhysicalID stays NULL
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	err := New(mock).UpsertThingEnrollmentWithDesiredVer(context.Background(), UpsertThingParams{
		ID: "thing-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
		EnrolledBy: "sso", Status: "online",
		Metadata: map[string]any{"bad": make(chan int)},
	}, 5)
	if err != nil {
		t.Fatalf("enrollment must survive unmarshalable metadata: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("metadata must be written as '{}': %v", err)
	}
}

// TestUpdateShadowReport_OutcomesMarshalErr covers the
// reported_outcomes marshal branch: an outcome timestamp outside
// JSON's representable year range fails serialization, and the report
// must be rejected before any SQL runs — half-writing reported without
// its outcome ledger would desynchronize the two.
func TestUpdateShadowReport_OutcomesMarshalErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// No ExpectExec: the marshal failure must short-circuit pre-SQL.
	badAt := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // beyond JSON year range
	err := New(mock).UpdateShadowReport(context.Background(), "thing-1",
		map[string]any{"k": "v"}, 1,
		map[string]ReportedKeyOutcome{"k": {AppliedAt: &badAt}})
	if err == nil {
		t.Fatal("expected outcomes marshal err")
	}
	if !strings.Contains(err.Error(), "marshal reported_outcomes") {
		t.Errorf("missing prefix: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no SQL may run after a marshal failure: %v", err)
	}
}

// TestListThings_ScanErrWraps covers the per-row scan branch of the
// paged list: a row shape that no longer matches the 29-column scan
// list must abort the listing with a wrapped "scan thing" error
// instead of returning a partial page that silently hides Things.
func TestListThings_ScanErrWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM thing_with_overrides`).
		WithArgs(50, 0). // default page size, first page
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("dev-1"))
	_, err := New(mock).ListThings(context.Background(), ListThingsParams{})
	if err == nil || !strings.Contains(err.Error(), "scan thing") {
		t.Errorf("expected wrapped scan err; got: %v", err)
	}
}

// TestFindDriftedThings_ScanErrWraps covers the drift job's per-row
// scan branch: a malformed row aborts the scan with a wrapped error so
// the job retries on the next tick rather than acting on a truncated
// drift set.
func TestFindDriftedThings_ScanErrWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`WHERE status IN \('online', 'drift'\)`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("dev-1"))
	_, err := New(mock).FindDriftedThings(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scan drifted") {
		t.Errorf("expected wrapped scan err; got: %v", err)
	}
}

// TestListDriftedThings_ScanErrWraps covers the API listing's per-row
// scan branch: a malformed row aborts with a wrapped error rather than
// serving a partial drift list to the admin UI.
func TestListDriftedThings_ScanErrWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`WHERE status = 'drift'`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("dev-1"))
	_, err := New(mock).ListDriftedThings(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scan drifted") {
		t.Errorf("expected wrapped scan err; got: %v", err)
	}
}

// TestUpdateDesiredForType_RowIterationErrors covers the two fan-out
// failure branches of the type-wide desired update: a per-row scan
// mismatch and a mid-iteration rows error must both abort the
// transaction-bound update with a wrapped error, so the caller rolls
// back instead of broadcasting a config_changed for a half-applied
// version bump.
func TestUpdateDesiredForType_RowIterationErrors(t *testing.T) {
	t.Run("scan err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		// One column instead of (id, desired_ver) → Scan fails.
		mock.ExpectQuery(`WITH next AS`).
			WithArgs("agent", "k", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("th-1"))
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		_, _, err := New(mock).UpdateDesiredForType(context.Background(), tx,
			"agent", "k", map[string]any{}, 0)
		if err == nil || !strings.Contains(err.Error(), "scan desired_ver") {
			t.Errorf("expected wrapped scan err; got: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("rows iteration err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("connection lost mid-stream")
		mock.ExpectBegin()
		mock.ExpectQuery(`WITH next AS`).
			WithArgs("agent", "k", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
				AddRow("th-1", int64(3)).
				CloseError(want))
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		_, _, err := New(mock).UpdateDesiredForType(context.Background(), tx,
			"agent", "k", map[string]any{}, 0)
		if !errors.Is(err, want) {
			t.Errorf("must propagate iteration err; got: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "iterate update results") {
			t.Errorf("missing prefix: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
}

// TestGetAttestationPubKeyWithExpiry_FailureBranches covers the two
// remaining error paths of the attestation-key lookup: a DB outage is
// wrapped (so CP can distinguish "retry later" from the not-found that
// triggers MITM fallback), and a stored key that is not valid base64
// is rejected instead of being handed to CP as garbage key bytes.
func TestGetAttestationPubKeyWithExpiry_FailureBranches(t *testing.T) {
	t.Run("infrastructure err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("conn reset")
		mock.ExpectQuery(`SELECT COALESCE`).
			WithArgs("thing-1").
			WillReturnError(want)
		_, _, err := New(mock).GetAttestationPubKeyWithExpiry(context.Background(), "thing-1")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "load attestation pubkey") {
			t.Errorf("missing prefix: %v", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Error("a DB outage must not be reported as not-found (would trigger MITM fallback)")
		}
	})
	t.Run("corrupt base64 key rejected", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COALESCE`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"publicKey", "certExpiresAt"}).
				AddRow("%%%not-base64%%%", ""))
		pub, _, err := New(mock).GetAttestationPubKeyWithExpiry(context.Background(), "thing-1")
		if err == nil || !strings.Contains(err.Error(), "decode attestation pubkey") {
			t.Errorf("expected decode err; got: %v", err)
		}
		if pub != nil {
			t.Errorf("no key bytes may be returned on decode failure; got %v", pub)
		}
	})
}
