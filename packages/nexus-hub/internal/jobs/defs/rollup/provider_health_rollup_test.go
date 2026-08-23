package rollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestProviderHealthRollup_Identity(t *testing.T) {
	j := NewProviderHealthRollup(nil, 0, testLogger())
	if j.ID() != providerHealthRollupJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != providerHealthRollupJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != 5*time.Minute {
		t.Errorf("default Interval = %v", j.Interval())
	}
	j2 := NewProviderHealthRollup(nil, 30*time.Second, testLogger())
	if j2.Interval() != 30*time.Second {
		t.Errorf("custom Interval = %v", j2.Interval())
	}
}

func TestProviderHealthRollup_Run_NoTraffic(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"pid", "pname", "total", "errors", "avg_latency_ms", "last_request_at", "last_error_at"}))

	j := &ProviderHealthRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestProviderHealthRollup_Run_ClassifiesStatuses(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	lastErr := now.Add(-2 * time.Minute)
	mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"pid", "pname", "total", "errors", "avg_latency_ms", "last_request_at", "last_error_at"}).
			AddRow("prov-1", "p1", int(100), int(1), int(120), now, &lastErr).  // healthy (1%)
			AddRow("prov-2", "p2", int(100), int(10), int(150), now, &lastErr). // degraded (10%)
			AddRow("prov-3", "p3", int(100), int(30), int(200), now, &lastErr)) // unavailable (30%)

	// One UPSERT per provider, and the third argument is the STATUS — which is
	// what this test is named for and previously did not assert at all.
	//
	// Two things made these expectations decorative. ExpectExec without
	// WithArgs means "expects ZERO arguments" in pgxmock, and upsert passes
	// nine, so none of them could ever match; and without ExpectationsWereMet
	// nothing reports an unmatched expectation. The Execs errored, Run logged
	// and continued, and the test passed green over a classification that never
	// reached the database.
	anyArg := pgxmock.AnyArg
	for _, want := range []string{"healthy", "degraded", "unavailable"} {
		mock.ExpectExec(`INSERT INTO "ProviderHealth"`).
			WithArgs(anyArg(), anyArg(), want, anyArg(), anyArg(),
				anyArg(), anyArg(), anyArg(), anyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	// The stale-verdict reset runs after the upserts on every pass.
	mock.ExpectExec(`UPDATE "ProviderHealth"`).WithArgs(anyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	j := &ProviderHealthRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a classification never reached the database: %v", err)
	}
}

func TestProviderHealthRollup_Run_CollectError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("query boom")
	mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).WillReturnError(sentinel)

	j := &ProviderHealthRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestProviderHealthRollup_Run_UpsertErrorLogsContinues(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"pid", "pname", "total", "errors", "avg_latency_ms", "last_request_at", "last_error_at"}).
			AddRow("prov-1", "p1", int(10), int(0), int(120), now, (*time.Time)(nil)))
	mock.ExpectExec(`INSERT INTO "ProviderHealth"`).WillReturnError(errors.New("upsert boom"))

	j := &ProviderHealthRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Errorf("Run should swallow upsert error, got %v", err)
	}
}
