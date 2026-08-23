package systemmetastore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

func TestGetSystemMetadata(t *testing.T) {
	s, m := newMock(t)
	// Happy: returns the stored JSON value.
	m.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).WithArgs("k1").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`{"a":1}`)))
	val, err := s.GetSystemMetadata(context.Background(), "k1")
	if err != nil || string(val) != `{"a":1}` {
		t.Fatalf("GetSystemMetadata: %s %v", val, err)
	}

	// Missing key → (nil, nil).
	m.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if val, err := s.GetSystemMetadata(context.Background(), "gone"); err != nil || val != nil {
		t.Fatalf("missing key should be (nil,nil): %s %v", val, err)
	}

	// DB error surfaces.
	m.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("x").WillReturnError(errors.New("boom"))
	if _, err := s.GetSystemMetadata(context.Background(), "x"); err == nil {
		t.Fatal("db error must surface")
	}
}

func TestSetSystemMetadata(t *testing.T) {
	s, m := newMock(t)
	// Happy upsert.
	m.ExpectExec(`INSERT INTO system_metadata`).WithArgs("k1", pgxmock.AnyArg(), "admin").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := s.SetSystemMetadata(context.Background(), "k1", map[string]int{"a": 1}, "admin"); err != nil {
		t.Fatalf("SetSystemMetadata: %v", err)
	}

	// Exec error.
	m.ExpectExec(`INSERT INTO system_metadata`).WithArgs("k1", pgxmock.AnyArg(), "admin").WillReturnError(errors.New("boom"))
	if err := s.SetSystemMetadata(context.Background(), "k1", 42, "admin"); err == nil {
		t.Fatal("exec error must surface")
	}

	// Marshal error: a channel is not JSON-serialisable → fails before any DB call.
	s2, _ := newMock(t)
	if err := s2.SetSystemMetadata(context.Background(), "k1", make(chan int), "admin"); err == nil {
		t.Fatal("marshal error must surface")
	}
}

// TestAuditUnknownKeys_ReportsOnlyWhatNobodyClaims.
//
// The query asks the database for every key NOT in the fixed registry, so what
// comes back is the fixed-key misses plus every dynamically-built key. The
// family filter then runs in Go. Both halves have to work: reporting a live
// `propagation_ledger:` row would make the boot WARN name a dozen healthy keys,
// and an operator who sees that twice stops reading it.
func TestAuditUnknownKeys_ReportsOnlyWhatNobodyClaims(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM system_metadata`).WithArgs(configkey.SystemMetadataKeys).
		WillReturnRows(pgxmock.NewRows([]string{"json_agg"}).AddRow([]byte(
			`["audit.payload","propagation_ledger:ai-gateway:models","proxy.killswitch"]`)))

	got, err := s.AuditUnknownKeys(context.Background())
	if err != nil {
		t.Fatalf("AuditUnknownKeys: %v", err)
	}
	want := map[string]bool{"audit.payload": true, "proxy.killswitch": true}
	for _, k := range got {
		if !want[k] {
			t.Errorf("%q reported; it is a live key built at runtime, and naming it teaches the "+
				"operator to ignore this warning", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("%q not reported — a key nobody reads, which is the whole subject", k)
	}
}

// An empty table is silence, not a finding: json_agg over no rows is the '[]'
// the COALESCE supplies, and a nil-vs-empty confusion here would log a WARN
// naming nothing on every boot of a fresh install.
func TestAuditUnknownKeys_EmptyTableReportsNothing(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM system_metadata`).WithArgs(configkey.SystemMetadataKeys).
		WillReturnRows(pgxmock.NewRows([]string{"json_agg"}).AddRow([]byte(`[]`)))

	got, err := s.AuditUnknownKeys(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v (err %v), want no findings", got, err)
	}
}

// A diagnostic that cannot run must say so rather than reporting "nothing
// unknown" — the caller logs the error and boots, but it must not read a
// failure as a clean bill of health.
func TestAuditUnknownKeys_QueryFailureIsNotACleanResult(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM system_metadata`).WithArgs(configkey.SystemMetadataKeys).
		WillReturnError(errors.New("connection reset"))

	got, err := s.AuditUnknownKeys(context.Background())
	if err == nil {
		t.Fatalf("a failed audit returned %v and no error, which reads as 'nothing unknown'", got)
	}
}

// A row that is not JSON is the same case: an error, not an empty answer.
func TestAuditUnknownKeys_UndecodableRowIsAnError(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM system_metadata`).WithArgs(configkey.SystemMetadataKeys).
		WillReturnRows(pgxmock.NewRows([]string{"json_agg"}).AddRow([]byte(`not json`)))

	if _, err := s.AuditUnknownKeys(context.Background()); err == nil {
		t.Fatal("an undecodable aggregate produced no error, so the boot would treat it as clean")
	}
}
