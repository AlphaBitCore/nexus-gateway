// Coverage-completion tests for the residual reachable branches in
// thing_registry.go that the existing focused suites did not yet drive:
// the GetThingStatus reader (WS service-token revocation check), the
// metadata-marshal fallback inside both enrollment upserts, the explicit
// JSON-"null" no-op in decodeJSONB, scan-error wraps on the row-iterating
// readers (ListThings / FindDriftedThings / ListDriftedThings /
// UpdateDesiredForType), and the generic-error / base64-decode-error tails
// of GetAttestationPubKeyWithExpiry.
//
// Each test asserts an observable outcome (the returned value, the specific
// error, or the not-found sentinel) — never bare execution.

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// TestGetThingStatus covers the WS service-token revocation reader: it must
// return the bare status string on hit, ErrNotFound when the id is unknown,
// and a wrapped error on a planner failure.
func TestGetThingStatus(t *testing.T) {
	t.Run("happy returns status", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT status FROM thing WHERE id = \$1`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("online"))
		s := New(mock)
		got, err := s.GetThingStatus(context.Background(), "thing-1")
		if err != nil {
			t.Fatalf("GetThingStatus: %v", err)
		}
		if got != "online" {
			t.Errorf("status = %q, want online", got)
		}
	})
	t.Run("unknown id → ErrNotFound", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT status FROM thing WHERE id = \$1`).
			WithArgs("missing").
			WillReturnError(pgx.ErrNoRows)
		s := New(mock)
		got, err := s.GetThingStatus(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
		if got != "" {
			t.Errorf("status = %q, want empty on not-found", got)
		}
	})
	t.Run("planner err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`SELECT status FROM thing WHERE id = \$1`).
			WithArgs("x").
			WillReturnError(want)
		s := New(mock)
		_, err := s.GetThingStatus(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get thing status") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

// unmarshalableMeta returns a metadata map whose value cannot be JSON-encoded
// (a channel), forcing json.Marshal(p.Metadata) to fail so the "{}" fallback
// branch in the enrollment upserts is exercised. The upsert must still issue
// its INSERT with an empty-object metadata payload rather than aborting.
func unmarshalableMeta() map[string]any {
	return map[string]any{"bad": make(chan int)}
}

// TestUpsertThingEnrollment_MetadataMarshalFallback drives the marshal-error
// branch: a non-encodable metadata value must NOT abort the enrollment — the
// row is still written with metadata defaulted to "{}".
func TestUpsertThingEnrollment_MetadataMarshalFallback(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// metadata arg must be the "{}" fallback, not the unmarshalable map.
	mock.ExpectExec(`INSERT INTO thing`).
		WithArgs(
			"a-1", "agent", "host-a", "1.0", "addr",
			"", "bearer", "http", "online",
			[]byte("{}"), // metadata fallback
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	s := New(mock)
	err := s.UpsertThingEnrollment(context.Background(), UpsertThingParams{
		ID: "a-1", Type: "agent", Name: "host-a", Version: "1.0", Address: "addr",
		AuthType: "bearer", ConnProtocol: "http", Status: "online",
		Metadata: unmarshalableMeta(),
	})
	if err != nil {
		t.Fatalf("UpsertThingEnrollment with bad metadata must still write: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected INSERT with {} metadata fallback: %v", err)
	}
}

// TestUpsertThingEnrollmentWithDesiredVer_MetadataMarshalFallback is the twin
// of the above for the desired-ver variant.
func TestUpsertThingEnrollmentWithDesiredVer_MetadataMarshalFallback(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`INSERT INTO thing`).
		WithArgs(
			"a-2", "agent", "host-b", "1.0", "addr",
			"", "bearer", "http", "online",
			[]byte("{}"), // metadata fallback
			pgxmock.AnyArg(),
			int64(5),
			nil, // physical_id: empty string → nil
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	s := New(mock)
	err := s.UpsertThingEnrollmentWithDesiredVer(context.Background(), UpsertThingParams{
		ID: "a-2", Type: "agent", Name: "host-b", Version: "1.0", Address: "addr",
		AuthType: "bearer", ConnProtocol: "http", Status: "online",
		Metadata: unmarshalableMeta(),
	}, 5)
	if err != nil {
		t.Fatalf("UpsertThingEnrollmentWithDesiredVer with bad metadata must still write: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected INSERT with {} metadata fallback: %v", err)
	}
}

// TestGetThing_DesiredNullNoop locks decodeJSONB's explicit string("null")
// branch: a literal JSON null in the desired column is a legitimate Postgres
// state and must decode to an empty/nil map WITHOUT surfacing an error (the
// registry only fails loudly on genuinely corrupt JSON).
func TestGetThing_DesiredNullNoop(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	row := []any{
		"thing-1", "agent", "host", "1.0", "addr",
		"sso", "bearer", "http",
		"online",
		[]byte("null"), // desired = JSON null → decodeJSONB no-op
		[]byte(`{}`),   // reported
		int64(1), int64(1),
		[]byte(`{}`), // metadata
		&now, now,
		[]byte(`{}`), // reported_outcomes
		(*time.Time)(nil),
		"", "", "", "", "",
		"", "", "", "",
	}
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("thing-1").
		WillReturnRows(pgxmock.NewRows(getThingCols).AddRow(row...))
	s := New(mock)
	got, err := s.GetThing(context.Background(), "thing-1")
	if err != nil {
		t.Fatalf("JSON-null desired must be a no-op, not an error: %v", err)
	}
	if len(got.Desired) != 0 {
		t.Errorf("Desired = %v, want empty for JSON null", got.Desired)
	}
}

// TestListThings_ScanErr drives the per-row Scan failure path: a row with too
// few columns makes the 29-target Scan fail, which must wrap as "scan thing".
func TestListThings_ScanErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	// Only one column vs the 29 the Scan expects → Scan errors.
	mock.ExpectQuery(`FROM thing_with_overrides`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t-1"))
	s := New(mock)
	_, err := s.ListThings(context.Background(), ListThingsParams{})
	if err == nil || !strings.Contains(err.Error(), "scan thing") {
		t.Errorf("expected scan-thing err; got: %v", err)
	}
}

// TestFindDriftedThings_ScanErr drives the Scan-failure wrap on the
// drift-detector reader.
func TestFindDriftedThings_ScanErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Two columns vs the six FindDriftedThings scans → Scan errors.
	mock.ExpectQuery(`FROM thing\s+WHERE status IN \('online', 'drift'\)`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("d-1", "agent"))
	s := New(mock)
	_, err := s.FindDriftedThings(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scan drifted") {
		t.Errorf("expected scan-drifted err; got: %v", err)
	}
}

// TestListDriftedThings_ScanErr drives the Scan-failure wrap on the API-facing
// drift reader (distinct SQL + an extra out_of_sync_keys column).
func TestListDriftedThings_ScanErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Three columns vs the seven ListDriftedThings scans → Scan errors.
	mock.ExpectQuery(`FROM thing\s+WHERE status = 'drift'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status"}).AddRow("d-1", "agent", "drift"))
	s := New(mock)
	_, err := s.ListDriftedThings(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scan drifted") {
		t.Errorf("expected scan-drifted err; got: %v", err)
	}
}

// TestUpdateDesiredForType_ScanErr drives the per-row Scan failure inside the
// RETURNING loop: a row missing the desired_ver column makes Scan(&id,
// &shadowDesiredVer) fail and wrap as "scan desired_ver".
func TestUpdateDesiredForType_ScanErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	// One column vs the two scanned → Scan errors inside the loop.
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "k", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t-1"))
	mock.ExpectRollback()
	tx, _ := mock.Begin(context.Background())
	s := New(mock)
	_, _, err := s.UpdateDesiredForType(context.Background(), tx, "agent", "k", map[string]any{"v": 1}, 0)
	if err == nil || !strings.Contains(err.Error(), "scan desired_ver") {
		t.Errorf("expected scan-desired_ver err; got: %v", err)
	}
	_ = tx.Rollback(context.Background())
}

// Note: the rows.Err() tail of UpdateDesiredForType ("iterate update results",
// lines 1066-1068) is a defensive post-loop check that pgxmock cannot drive —
// RowError surfaces during the in-loop Scan (covered by
// TestUpdateDesiredForType_ScanErr), never as a deferred Err() after a clean
// iteration. Left as genuinely-unreachable-under-mock residual; the loop body
// itself is fully covered.

// TestGetAttestationPubKeyWithExpiry_ErrorTails covers the two remaining tails
// of the attestation getter: a generic (non-ErrNoRows) query error must wrap as
// "load attestation pubkey", and a stored publicKey that is not valid base64
// must wrap as "decode attestation pubkey".
func TestGetAttestationPubKeyWithExpiry_ErrorTails(t *testing.T) {
	t.Run("generic query err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`SELECT COALESCE`).
			WithArgs("thing-1").
			WillReturnError(want)
		s := New(mock)
		_, _, err := s.GetAttestationPubKeyWithExpiry(context.Background(), "thing-1")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "load attestation pubkey") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("invalid base64 publicKey wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		// "!!!" is non-empty (so not a miss) but not valid base64 → decode err.
		mock.ExpectQuery(`SELECT COALESCE`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"publicKey", "certExpiresAt"}).
				AddRow("!!!not-base64!!!", ""))
		s := New(mock)
		_, _, err := s.GetAttestationPubKeyWithExpiry(context.Background(), "thing-1")
		if err == nil || !strings.Contains(err.Error(), "decode attestation pubkey") {
			t.Errorf("expected decode err; got: %v", err)
		}
	})
}
