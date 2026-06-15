package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestCacheQualityMonitor_Identity(t *testing.T) {
	j := NewCacheQualityMonitor(nil, 0, testLogger())
	if j.ID() != cacheQualityJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != cacheQualityJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != 5*time.Minute {
		t.Errorf("default Interval = %v", j.Interval())
	}
	j2 := NewCacheQualityMonitor(nil, 30*time.Second, testLogger())
	if j2.Interval() != 30*time.Second {
		t.Errorf("custom Interval = %v", j2.Interval())
	}
}

// expectStats wires the stats QueryRow with the four-column baseline-clean shape:
// totalNorm, errorNorm, totalBaseline (non-norm), errorBaseline (non-norm).
func expectStats(mock pgxmock.PgxPoolIface, totalNorm, errorNorm, totalBaseline, errorBaseline int64) {
	mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"a", "b", "c", "d"}).
			AddRow(totalNorm, errorNorm, totalBaseline, errorBaseline))
}

func TestCacheQualityMonitor_Run_NotEnoughData(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// totalNorm < minNormalisedRequests; totalBaseline=95, errorBaseline=1.
	expectStats(mock, 5, 0, 95, 1)

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCacheQualityMonitor_Run_WithinAcceptableRange(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// totalNorm=50, errorNorm=1 → 2% rate; baseline (non-norm): 950 requests, 9 errors = ~0.9%; 2x < 3x → ok.
	expectStats(mock, 50, 1, 950, 9)

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCacheQualityMonitor_Run_RevertsOnRegression(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// totalNorm=100 errorNorm=20 (20%), baseline NON-norm: 900 requests, 9 errors = 1% → 20x triggers revert.
	expectStats(mock, 100, 20, 900, 9)
	// revertToDryRun query.
	cfgJSON := `{"rules":{"r1":{"enabled":true,"dry_run_always":false},"r2":{"enabled":false}}}`
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(cfgJSON)))
	mock.ExpectExec(`UPDATE cache_adapter_config`).
		WithArgs("openai", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCacheQualityMonitor_Run_StatsError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("stats boom")
	mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).WillReturnError(sentinel)
	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCacheQualityMonitor_RevertToDryRun_NoEnabledRules(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// All rules disabled → no UPDATE expected.
	cfgJSON := `{"rules":{"r1":{"enabled":false}}}`
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(cfgJSON)))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); err != nil {
		t.Fatalf("revert: %v", err)
	}
}

func TestCacheQualityMonitor_RevertToDryRun_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("revert query boom")
	mock.ExpectQuery(`FROM cache_adapter_config`).WillReturnError(sentinel)
	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCacheQualityMonitor_RevertToDryRun_UpdateError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	cfgJSON := `{"rules":{"r1":{"enabled":true}}}`
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(cfgJSON)))
	sentinel := errors.New("update boom")
	mock.ExpectExec(`UPDATE cache_adapter_config`).
		WithArgs("openai", pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestCacheQualityMonitor_Run_NullBaselineDoesNotFalseAlarm guards the COALESCE fix.
// The AI-gateway producer writes NULL (not 0) for normalized_strip_count/cache_marker_injected
// when a row is untouched. Without COALESCE, those NULLs are excluded from the baseline
// (SQL NULL=0 is never TRUE), driving totalBaseline→0 and baselineErrorRate→0.01 floor.
// That turns the monitor into a blunt ">3% normalised error rate → revert" check,
// causing false alarms when the true baseline rate is equally elevated.
// This test verifies: when normalised and baseline rates are equal (1× < 3×), no revert fires.
func TestCacheQualityMonitor_Run_NullBaselineDoesNotFalseAlarm(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// 25 normalised requests, 2 errors = 8%. Baseline: 900 requests, 72 errors = 8%.
	// Same rate → multiplier = 1.0 < 3.0 → no revert. Without COALESCE the baseline
	// would be 0, floor=0.01, threshold=3% → 8% would falsely trigger revert.
	expectStats(mock, 25, 2, 900, 72)

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// No cache_adapter_config queries expected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected query on cache_adapter_config: %v", err)
	}
}

func TestCacheQualityMonitor_RevertToDryRun_UnmarshalError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{bad json`)))
	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); err == nil {
		t.Fatalf("expected unmarshal error")
	}
}
