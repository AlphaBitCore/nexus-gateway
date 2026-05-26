package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestThingRollup5m_Identity(t *testing.T) {
	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	if j.ID() == "" {
		t.Error("ID empty")
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
}

func TestThingRollup5m_Run_NoMergeNeeded(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(time.Now().UTC()))

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestThingRollup5m_ColdStartWatermark_EarliestErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM traffic_event`).WillReturnError(context.Canceled)
	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock
	got := j.coldStartWatermark(context.Background())
	if got.IsZero() {
		t.Errorf("coldStartWatermark returned zero time on error path")
	}
}

func TestThingRollup5m_ProcessOneBucket_BeginError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin().WillReturnError(context.Canceled)

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock
	if err := j.processOneBucket(context.Background(), time.Now().UTC().Truncate(5*time.Minute)); err == nil {
		t.Fatalf("expected error")
	}
}
