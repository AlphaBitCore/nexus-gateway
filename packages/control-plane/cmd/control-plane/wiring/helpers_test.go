package wiring

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

var (
	opsMetricsOnce sync.Once
	opsMetricsReg  *metricsreg.Registry
)

// getOpsMetricsRegistry returns the package-level ops metrics registry,
// calling InitOpsMetrics exactly once to avoid duplicate Prometheus registration.
func getOpsMetricsRegistry() *metricsreg.Registry {
	opsMetricsOnce.Do(func() {
		opsMetricsReg = InitOpsMetrics()
	})
	return opsMetricsReg
}

// TestMain initialises shared test state (ops metrics registry) once for the
// whole package test run so individual tests don't trigger duplicate Prometheus
// registration panics.
func TestMain(m *testing.M) {
	// Register metrics once before any test runs.
	getOpsMetricsRegistry()
	os.Exit(m.Run())
}

// silentLogger returns a logger that discards all output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeMQProducer is a no-op mq.Producer for wiring tests that require a
// non-nil producer without a real NATS connection.
type fakeMQProducer struct{}

func (f *fakeMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (f *fakeMQProducer) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (f *fakeMQProducer) Close() error                                        { return nil }

// fakeMQConsumer is a no-op mq.Consumer for wiring tests.
type fakeMQConsumer struct{}

func (f *fakeMQConsumer) Subscribe(_ context.Context, _ string, _ mq.MessageHandler) error {
	return nil
}
func (f *fakeMQConsumer) Consume(_ context.Context, _ string, _ string, _ mq.MessageHandler) error {
	return nil
}
func (f *fakeMQConsumer) Close() error { return nil }
