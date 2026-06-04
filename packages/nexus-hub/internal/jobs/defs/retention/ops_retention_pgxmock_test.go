// Pgxmock-driven Run() tests for OpsRetentionJob. Covers loadLayers,
// purgeLayer dispatch (all known layers + unknown fallback), and the
// chunked-DELETE loops in deleteLooping / deleteDiag.

package retention

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestOpsRetention_Run_NoLayers(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days"}))

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRetention_Run_LoadError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("load boom")
	mock.ExpectQuery(`FROM metric_ops_retention_config`).WillReturnError(sentinel)

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRetention_Run_DisabledLayersSkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days"}).
			AddRow("runtime_5m", int(0)).
			AddRow("business_5m", int(-1)))

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRetention_Run_UnknownLayerIgnored(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days"}).
			AddRow("totally-unknown", int(7)))

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRetention_Run_AllLayersHappy(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Every supported layer with 7-day retention. Each DELETE returns 0
	// rows so the per-layer loop exits immediately (single iteration).
	mock.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days"}).
			AddRow("runtime_5m", int(7)).
			AddRow("business_5m", int(7)).
			AddRow("runtime_1h", int(7)).
			AddRow("business_1h", int(7)).
			AddRow("runtime_1d", int(7)).
			AddRow("business_1d", int(7)).
			AddRow("runtime_1mo", int(7)).
			AddRow("business_1mo", int(7)).
			AddRow("diag_info", int(7)).
			AddRow("diag_warn", int(7)).
			AddRow("diag_error", int(7)).
			AddRow("diag_fatal", int(7)))

	// 8 looping deletes (raw + rollup tiers) + 4 diag deletes.
	for range 8 {
		mock.ExpectExec(`DELETE FROM`).
			WithArgs(pgxmock.AnyArg(), opsRetentionDeleteLimit).
			WillReturnResult(pgxmock.NewResult("DELETE", 0))
	}
	for range 4 {
		mock.ExpectExec(`DELETE FROM thing_diag_event`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), opsRetentionDeleteLimit).
			WillReturnResult(pgxmock.NewResult("DELETE", 0))
	}

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestOpsRetention_Run_LayerPurgeErrorJoinedAndOthersContinue(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days"}).
			AddRow("runtime_5m", int(7)).
			AddRow("business_5m", int(7)))

	sentinel := errors.New("delete boom")
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), opsRetentionDeleteLimit).
		WillReturnError(sentinel)
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), opsRetentionDeleteLimit).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRetention_Run_DeleteLoopsUntilDrained(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days"}).
			AddRow("diag_warn", int(7)))
	// First iteration returns full batch → loop continues.
	mock.ExpectExec(`DELETE FROM thing_diag_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), opsRetentionDeleteLimit).
		WillReturnResult(pgxmock.NewResult("DELETE", opsRetentionDeleteLimit))
	// Second iteration short → loop exits.
	mock.ExpectExec(`DELETE FROM thing_diag_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), opsRetentionDeleteLimit).
		WillReturnResult(pgxmock.NewResult("DELETE", 50))

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestOpsRetention_LoadLayers_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("iter boom")
	mock.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days"}).
			AddRow("runtime_5m", int(7)).RowError(0, sentinel))

	j := &OpsRetentionJob{pool: mock, logger: testLogger()}
	_, err := j.loadLayers(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
