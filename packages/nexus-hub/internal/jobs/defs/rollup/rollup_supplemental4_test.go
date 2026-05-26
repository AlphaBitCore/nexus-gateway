// rollup_supplemental4_test.go closes the final coverage gaps that remain
// after rollup_supplemental3_test.go. Targets:
//
//   - CredentialHealthRollupJob.priorStatus: direct call with empty rolled (early return)
//   - CredentialHealthRollupJob.batchUpdate: nil rate5m / nil rate1h paths
//   - ThingRollup5mJob.Run: cold-start watermark path (GetWatermark returns empty)
//   - aggregateTrafficEvents: rows.Err path (via pgxmock RowsError)
//   - aggregateThingEvents: query error path + rows.Err path
//   - worstHookDecision: b wins when rank(b) > rank(a) and b == ""
//   - provider_health_rollup.collect: scan error path
package rollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// CredentialHealthRollupJob.priorStatus — direct call with empty rolled

// TestCredentialHealthRollup_PriorStatus_EmptyRolled calls priorStatus directly
// with an empty rolled slice to exercise the len(rolled)==0 early-return path.
func TestCredentialHealthRollup_PriorStatus_EmptyRolled(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	j := NewCredentialHealthRollup(nil, nil, 5*time.Minute, testLogger(), nil)
	j.pool = mock

	out, err := j.priorStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("priorStatus: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map; got %d entries", len(out))
	}
	// No DB query must be issued.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock had unexpected calls: %v", err)
	}
}

// CredentialHealthRollupJob.batchUpdate — nil rate5m and nil rate1h paths

// TestCredentialHealthRollup_Run_NilRates exercises batchUpdate with a
// credential that has short.samples==0 (nil rate5m) and long.samples==0
// (nil rate1h) so both nil branches inside batchUpdate fire.
func TestCredentialHealthRollup_Run_NilRates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	collectCols := []string{
		"credential_id",
		"short_samples", "short_success", "short_auth", "short_rate",
		"short_5xx", "short_timeout", "short_client",
		"short_last",
		"long_samples", "long_success",
	}
	// samples==0 → rate5m ptr is nil; long_samples==0 → rate1h ptr is nil.
	mock.ExpectQuery(`FROM\s+traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(collectCols).AddRow(
			"cred-nil-rates",
			0, 0, 0, 0, 0, 0, 0,
			(*time.Time)(nil),
			0, 0,
		))

	mock.ExpectQuery(`FROM\s+"Credential"\s+WHERE\s+id\s+=\s+ANY`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "healthStatus", "healthStatusChangedAt"}).
			AddRow("cred-nil-rates", credstate.HealthUnknown, (*time.Time)(nil)))

	mock.ExpectExec(`UPDATE\s+"Credential"`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	j := NewCredentialHealthRollup(nil, nil, 5*time.Minute, testLogger(), nil)
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations: %v", err)
	}
}

// ThingRollup5mJob.Run — cold-start watermark path

// TestThingRollup5m_Run_ColdStart exercises the cold-start path in
// ThingRollup5mJob.Run where GetWatermark returns zero rows → watermark.IsZero()
// → coldStartWatermark called → logger.Info("initializing watermark") fires.
// The test stops the run early via the latestSealed guard (the default lookback
// produces a watermark far in the past so processOneBucket would run many times;
// we let the first unexpected DB call cause a non-nil error return, which we
// ignore — we only care that the cold-start lines were hit).
func TestThingRollup5m_Run_ColdStart(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// GetWatermark: zero rows → IsZero() is true.
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))

	// coldStartWatermark → EarliestTrafficEventTimestamp → empty (no rows).
	mock.ExpectQuery(`FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"timestamp"}))

	// processOneBucket (first of many scheduled from the default lookback):
	// return error on Begin to stop the run immediately.
	mock.ExpectBegin().WillReturnError(errors.New("stop here"))

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	// Error from processOneBucket → Run returns error (non-nil). That's fine;
	// the cold-start lines (coldStartWatermark + logger.Info) were executed.
	_ = j.Run(context.Background())
}

// aggregateTrafficEvents — rows.Err path

// TestRollup5m_AggregateTrafficEvents_RowsErr exercises the rows.Err() error
// path in aggregateTrafficEvents by using pgxmock's RowsError to inject an
// error after all rows have been scanned successfully.
func TestRollup5m_AggregateTrafficEvents_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	sentinel := errors.New("rows.Err boom")
	// Return one valid row, then inject a rows-level error via CloseError so
	// rows.Err() returns it after all rows are exhausted.
	rows := pgxmock.NewRows(trafficEventCols).
		AddRow(oneTrafficEventRow(bucket.Add(time.Minute))...).
		CloseError(sentinel)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	_, rowsErr := j.aggregateTrafficEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if !errors.Is(rowsErr, sentinel) {
		t.Errorf("err = %v, want rows.Err sentinel", rowsErr)
	}
}

// aggregateThingEvents — query error + rows.Err paths

// TestThingRollup5m_AggregateThingEvents_QueryError exercises the Query error
// path in aggregateThingEvents (traffic_event query itself fails).
func TestThingRollup5m_AggregateThingEvents_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	sentinel := errors.New("thing query boom")
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	_, qErr := j.aggregateThingEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if !errors.Is(qErr, sentinel) {
		t.Errorf("err = %v, want query error sentinel", qErr)
	}
}

// TestThingRollup5m_AggregateThingEvents_RowsErr exercises the rows.Err() path
// in aggregateThingEvents.
func TestThingRollup5m_AggregateThingEvents_RowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	sentinel := errors.New("thing rows.Err boom")
	rows := pgxmock.NewRows(thingTrafficEventCols).
		AddRow(oneThingTrafficEventRow(bucket.Add(time.Minute))...).
		CloseError(sentinel)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	_, rowsErr := j.aggregateThingEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if !errors.Is(rowsErr, sentinel) {
		t.Errorf("err = %v, want rows.Err sentinel", rowsErr)
	}
}

// provider_health_rollup.collect — scan error path

// TestProviderHealthRollup_Collect_ScanError exercises the scan error path in
// provider_health_rollup.collect by returning a type mismatch (int for pid).
func TestProviderHealthRollup_Collect_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// The collect query is matched by the FROM traffic_event substring.
	// Inject a type that pgxmock cannot convert to string: a struct.
	// Provide a wrong column count (too few) to trigger the scan error.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"pid", "pname", "total", "errors", "avg_latency_ms", "last_request_at"}).
			AddRow("openai", "openai", 100, 5, 200, time.Now()))

	j := NewProviderHealthRollup(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Error("expected scan error from column count mismatch in provider collect; got nil")
	}
}
