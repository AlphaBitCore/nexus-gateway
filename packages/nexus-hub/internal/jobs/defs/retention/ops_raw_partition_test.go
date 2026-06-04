// Pgxmock-driven tests for OpsRawPartitionJob. Cover identity/defaults,
// ensurePartitions (CREATE per offset + error), dropAgedPartitions (list →
// parse → conditional DROP, plus query/scan/drop error paths), and the
// parseOpsRawPartitionDate name parser. No live Postgres is needed.
package retention

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestOpsRawPartition_Identity(t *testing.T) {
	j := NewOpsRawPartition(nil, 6*time.Hour, 30, testLogger())
	if j.ID() != "ops-raw-partition" {
		t.Errorf("ID = %q, want ops-raw-partition", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 6*time.Hour {
		t.Errorf("Interval = %v, want 6h", j.Interval())
	}
	if !j.RunOnStart() {
		t.Error("RunOnStart should be true (current-day partition must exist before first tick)")
	}
}

func TestOpsRawPartition_Defaults(t *testing.T) {
	// interval <= 0 → 6h; retentionDays <= 0 → 30.
	j := NewOpsRawPartition(nil, 0, 0, testLogger())
	if j.Interval() != 6*time.Hour {
		t.Errorf("Interval = %v, want 6h default", j.Interval())
	}
	if j.retentionDays != 30 {
		t.Errorf("retentionDays = %d, want 30 default", j.retentionDays)
	}
}

// expectEnsurePartitions registers the four idempotent CREATE TABLE execs
// (offsets -1,0,1,2) that ensurePartitions issues.
func expectEnsurePartitions(mock pgxmock.PgxPoolIface) {
	for range opsRawAheadOffsets {
		mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS metric_ops_raw_p")).
			WillReturnResult(pgxmock.NewResult("CREATE", 0))
	}
}

// TestOpsRawPartition_Run_CreatesAndDropsAged pre-creates the partition window
// and drops only partitions whose whole day predates the retention cutoff,
// leaving future partitions and non-managed relations untouched.
func TestOpsRawPartition_Run_CreatesAndDropsAged(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectEnsurePartitions(mock)

	// List partitions: one ancient (drop), one non-managed relname (skip),
	// one wrong-length suffix (skip), one far-future (skip).
	rows := pgxmock.NewRows([]string{"relname"}).
		AddRow("metric_ops_raw_p20000101").
		AddRow("some_unrelated_table").
		AddRow("metric_ops_raw_p2000").
		AddRow("metric_ops_raw_p29991231")
	mock.ExpectQuery(regexp.QuoteMeta("pg_inherits")).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)

	// Only the ancient partition is dropped.
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE IF EXISTS metric_ops_raw_p20000101")).
		WillReturnResult(pgxmock.NewResult("DROP", 0))

	j := NewOpsRawPartition(nil, 6*time.Hour, 30, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestOpsRawPartition_Run_EnsureError surfaces a partition-create failure with
// the offending partition named.
func TestOpsRawPartition_Run_EnsureError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	wantErr := errors.New("disk full")
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS metric_ops_raw_p")).
		WillReturnError(wantErr)

	j := NewOpsRawPartition(nil, 6*time.Hour, 30, testLogger())
	j.pool = mock
	err = j.Run(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run err = %v, want wrap of %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "ensure partitions") {
		t.Fatalf("Run err = %q, want mention of ensure partitions", err.Error())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestOpsRawPartition_Run_ListError surfaces a partition-listing failure.
func TestOpsRawPartition_Run_ListError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	wantErr := errors.New("catalog unavailable")
	expectEnsurePartitions(mock)
	mock.ExpectQuery(regexp.QuoteMeta("pg_inherits")).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(wantErr)

	j := NewOpsRawPartition(nil, 6*time.Hour, 30, testLogger())
	j.pool = mock
	err = j.Run(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run err = %v, want wrap of %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "list partitions") {
		t.Fatalf("Run err = %q, want mention of list partitions", err.Error())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestOpsRawPartition_Run_ScanError surfaces a row-scan failure while iterating
// the partition list.
func TestOpsRawPartition_Run_ScanError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	wantErr := errors.New("scan blew up")
	expectEnsurePartitions(mock)
	rows := pgxmock.NewRows([]string{"relname"}).
		AddRow("metric_ops_raw_p20000101").
		RowError(0, wantErr)
	mock.ExpectQuery(regexp.QuoteMeta("pg_inherits")).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)

	j := NewOpsRawPartition(nil, 6*time.Hour, 30, testLogger())
	j.pool = mock
	err = j.Run(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run err = %v, want wrap of %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "scan partition name") {
		t.Fatalf("Run err = %q, want mention of scan partition name", err.Error())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestOpsRawPartition_Run_DropError joins DROP failures (the job keeps going and
// returns the joined error rather than aborting).
func TestOpsRawPartition_Run_DropError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	wantErr := errors.New("drop denied")
	expectEnsurePartitions(mock)
	rows := pgxmock.NewRows([]string{"relname"}).
		AddRow("metric_ops_raw_p20000101")
	mock.ExpectQuery(regexp.QuoteMeta("pg_inherits")).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE IF EXISTS metric_ops_raw_p20000101")).
		WillReturnError(wantErr)

	j := NewOpsRawPartition(nil, 6*time.Hour, 30, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Run err = %v, want join of %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestParseOpsRawPartitionDate(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantOK bool
		want   time.Time
	}{
		{"valid", "metric_ops_raw_p20260531", true, time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)},
		{"wrong prefix", "metric_ops_rollup_p20260531", false, time.Time{}},
		{"wrong length", "metric_ops_raw_p2026", false, time.Time{}},
		{"bad date", "metric_ops_raw_p20261332", false, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOpsRawPartitionDate(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("date = %v, want %v", got, tc.want)
			}
		})
	}
}
