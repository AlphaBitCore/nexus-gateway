package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestDataRetention_Identity(t *testing.T) {
	j := NewDataRetention(nil, DataRetentionConfig{TrafficEventDays: 90}, 24*time.Hour, testLogger())
	if j.ID() != "data-retention" {
		t.Errorf("ID = %q, want data-retention", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h", j.Interval())
	}
}

func TestDataRetention_IntervalDefault(t *testing.T) {
	j := NewDataRetention(nil, DataRetentionConfig{}, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h default", j.Interval())
	}
	j2 := NewDataRetention(nil, DataRetentionConfig{}, -time.Second, testLogger())
	if j2.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h default for negative", j2.Interval())
	}
}

// TestDataRetention_PayloadPurgeConfig locks in the new
// TrafficEventPayloadDays field: it must be independently configurable and
// default to disabled (0) when the caller does not set it, matching the
// behaviour of the other three retention knobs. Live SQL execution is
// covered by the e2e harness; here we only guard the zero-value semantics
// and config-plumbing contract.
func TestDataRetention_PayloadPurgeConfig(t *testing.T) {
	// Zero → disabled (consistent with TrafficEventDays / AdminAuditLogDays).
	disabled := NewDataRetention(nil, DataRetentionConfig{}, 24*time.Hour, testLogger())
	if got := disabled.cfg.TrafficEventPayloadDays; got != 0 {
		t.Errorf("zero-value TrafficEventPayloadDays = %d, want 0 (disabled)", got)
	}

	// Explicit value survives round-trip into the job config; the 30-day
	// default lives in nexus-hub config.DefaultConfig, not here — this job
	// only honours whatever its caller passes in.
	enabled := NewDataRetention(nil, DataRetentionConfig{TrafficEventPayloadDays: 30}, 24*time.Hour, testLogger())
	if got := enabled.cfg.TrafficEventPayloadDays; got != 30 {
		t.Errorf("TrafficEventPayloadDays = %d, want 30", got)
	}
}

func TestDataRetention_Run_AllConfigured(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM traffic_event_payload`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 12))
	mock.ExpectExec(`DELETE FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 5))
	mock.ExpectExec(`DELETE FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))
	mock.ExpectExec(`DELETE FROM metric_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 7))

	cfg := DataRetentionConfig{
		TrafficEventDays:        30,
		TrafficEventPayloadDays: 7,
		AdminAuditLogDays:       90,
		MetricRollupDays:        14,
	}
	j := &DataRetentionJob{pool: mock, cfg: cfg, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestDataRetention_Run_AllDisabled(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	// Zero-value config: no queries expected at all.

	j := &DataRetentionJob{pool: mock, cfg: DataRetentionConfig{}, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestDataRetention_Run_PartialError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("boom")
	mock.ExpectExec(`DELETE FROM traffic_event_payload`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectExec(`DELETE FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 5))
	mock.ExpectExec(`DELETE FROM "AdminAuditLog"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))
	mock.ExpectExec(`DELETE FROM metric_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	cfg := DataRetentionConfig{TrafficEventDays: 1, TrafficEventPayloadDays: 1, AdminAuditLogDays: 1, MetricRollupDays: 1}
	j := &DataRetentionJob{pool: mock, cfg: cfg, logger: testLogger()}
	err = j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel joined", err)
	}
}
