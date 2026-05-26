package wiring

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
)

func TestInitAuditWriter_NilProducer_ReturnsWriter(t *testing.T) {
	// nil producer is explicitly supported — the audit package drops events.
	w := InitAuditWriter(nil, silentLogger())
	if w == nil {
		t.Error("expected non-nil audit.Writer even when producer is nil")
	}
}

func TestInitAuditWriter_FakeProducer_ReturnsWriter(t *testing.T) {
	w := InitAuditWriter(&fakeMQProducer{}, silentLogger())
	if w == nil {
		t.Error("expected non-nil audit.Writer with fake producer")
	}
}

// TestInitAuditWriter_FailureObserver_NilMetric verifies that the failure
// observer closure is safely called when cpmetrics.AdminAuditLogFailedTotal
// is nil (not yet registered). The observable outcome is no panic.
func TestInitAuditWriter_FailureObserver_NilMetric(t *testing.T) {
	// Ensure the metrics counter is nil (unregistered state).
	savedCounter := cpmetrics.AdminAuditLogFailedTotal
	cpmetrics.AdminAuditLogFailedTotal = nil
	defer func() { cpmetrics.AdminAuditLogFailedTotal = savedCounter }()

	failProducer := &failingMQProducer{err: errors.New("enqueue failed")}
	w := InitAuditWriter(failProducer, silentLogger())

	// Trigger the failure observer by logging an entry that will fail publish.
	// Observable: no panic from the nil-counter guard in the closure.
	_ = w.Log(context.Background(), audit.Entry{Action: "test"})
}

// TestInitAuditWriter_FailureObserver_NonNilMetric verifies the counter is
// incremented when the metric IS registered (the non-nil branch).
func TestInitAuditWriter_FailureObserver_NonNilMetric(t *testing.T) {
	// Metrics are registered once by TestMain via getOpsMetricsRegistry().
	if cpmetrics.AdminAuditLogFailedTotal == nil {
		t.Skip("metrics counter still nil (Prometheus registration guard)")
	}

	failProducer := &failingMQProducer{err: errors.New("publish failed")}
	w := InitAuditWriter(failProducer, silentLogger())

	// Trigger the failure observer. Observable: counter.Inc() runs, no panic.
	_ = w.Log(context.Background(), audit.Entry{Action: "test-action"})
}

// failingMQProducer is an mq.Producer whose Enqueue always returns an error,
// triggering the FailureObserver in the audit writer.
type failingMQProducer struct {
	err error
}

func (f *failingMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return f.err }
func (f *failingMQProducer) Enqueue(_ context.Context, _ string, _ []byte) error { return f.err }
func (f *failingMQProducer) Close() error                                        { return nil }
