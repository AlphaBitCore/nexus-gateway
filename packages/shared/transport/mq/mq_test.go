package mq_test

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

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

// metricsNamespaceSeq makes each NewMetrics call in this package use a namespace
// nothing has registered before.
//
// The previous constant namespace was labelled "unique to avoid duplicate
// Prometheus registration across test runs" and was not: promauto registers into
// a process-wide registry, so the SECOND run of this test in one process — any
// `-count=2`, which is how a flaky suite gets shaken out — panicked on duplicate
// registration. A constant is unique across processes, and the collision is
// within one.
var metricsNamespaceSeq atomic.Uint64

func TestNewMetrics_RegistersAllCounters(t *testing.T) {
	ns := fmt.Sprintf("test_mq_counters_%d", metricsNamespaceSeq.Add(1))
	m := mq.NewMetrics(ns)

	// Assert the counters are REGISTERED under the names an operator's dashboard
	// queries, not merely that the struct fields are non-nil. A field can be
	// non-nil while carrying the wrong namespace, subsystem or name — which is
	// exactly the kind of change that silently empties a dashboard, and the kind a
	// nil-check cannot see.
	want := []string{
		ns + "_mq_published_total",
		ns + "_mq_enqueued_total",
		ns + "_mq_consumed_total",
		ns + "_mq_acked_total",
		ns + "_mq_naked_total",
		ns + "_mq_deferred_total",
		ns + "_mq_errors_total",
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	registered := map[string]bool{}
	for _, f := range families {
		registered[f.GetName()] = true
	}
	for _, name := range want {
		if !registered[name] {
			t.Errorf("%s is not registered — the counter exists on the struct but nothing "+
				"scraping this process can see it", name)
		}
	}

	// The struct must also hand each counter back, or the code that increments it
	// writes to a nil pointer at runtime.
	for name, c := range map[string]prometheus.Counter{
		"PublishedTotal": m.PublishedTotal,
		"EnqueuedTotal":  m.EnqueuedTotal,
		"ConsumedTotal":  m.ConsumedTotal,
		"AckedTotal":     m.AckedTotal,
		"NakedTotal":     m.NakedTotal,
		"DeferredTotal":  m.DeferredTotal,
		"ErrorsTotal":    m.ErrorsTotal,
	} {
		if c == nil {
			t.Errorf("%s is nil", name)
		}
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
