package mq_test

import (
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

func TestNewProducer_UnknownDriver(t *testing.T) {
	_, err := mq.NewProducer(mq.Config{Driver: "unknown"}, slog.Default())
	if err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

func TestNewConsumer_UnknownDriver(t *testing.T) {
	_, err := mq.NewConsumer(mq.Config{Driver: "unknown"}, slog.Default())
	if err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

func TestNewMetrics_RegistersAllCounters(t *testing.T) {
	// Unique namespace to avoid duplicate Prometheus registration across test runs.
	m := mq.NewMetrics("test_mq_s1_counters")
	if m.PublishedTotal == nil {
		t.Error("PublishedTotal is nil")
	}
	if m.EnqueuedTotal == nil {
		t.Error("EnqueuedTotal is nil")
	}
	if m.ConsumedTotal == nil {
		t.Error("ConsumedTotal is nil")
	}
	if m.AckedTotal == nil {
		t.Error("AckedTotal is nil")
	}
	if m.NakedTotal == nil {
		t.Error("NakedTotal is nil")
	}
	if m.DeferredTotal == nil {
		t.Error("DeferredTotal is nil")
	}
	if m.ErrorsTotal == nil {
		t.Error("ErrorsTotal is nil")
	}
}

func TestRegisterDriver_ThenCreate(t *testing.T) {
	const testDriver = "test_stub_s1"

	mq.RegisterDriver(testDriver,
		func(cfg mq.Config, logger *slog.Logger) (mq.Producer, error) { return nil, nil },
		func(cfg mq.Config, logger *slog.Logger) (mq.Consumer, error) { return nil, nil },
	)

	p, err := mq.NewProducer(mq.Config{Driver: testDriver}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Error("expected nil producer from stub factory")
	}

	c, err := mq.NewConsumer(mq.Config{Driver: testDriver}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c != nil {
		t.Error("expected nil consumer from stub factory")
	}
}
