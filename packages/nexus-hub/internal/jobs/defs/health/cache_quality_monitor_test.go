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

// expectStats wires the stats QueryRow with given counts.
func expectStats(mock pgxmock.PgxPoolIface, totalNorm, errorNorm, totalAll, errorAll int64) {
	mock.ExpectQuery(`FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"a", "b", "c", "d"}).
			AddRow(totalNorm, errorNorm, totalAll, errorAll))
}

func TestCacheQualityMonitor_Run_NotEnoughData(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	expectStats(mock, 5, 0, 100, 1) // totalNorm < minNormalisedRequests

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCacheQualityMonitor_Run_WithinAcceptableRange(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// totalNorm=50, errorNorm=1 → 2% rate; baseline 1%; 2x not above 3x → ok.
	expectStats(mock, 50, 1, 1000, 10)

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCacheQualityMonitor_Run_RevertsOnRegression(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// totalNorm=100 errorNorm=20 (20%), baseline (totalAll=1000 errorAll=10 = 1%) → 20x.
	expectStats(mock, 100, 20, 1000, 10)
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
