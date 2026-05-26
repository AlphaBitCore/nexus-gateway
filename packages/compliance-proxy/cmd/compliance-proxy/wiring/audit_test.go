package wiring

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// stubMQProducer is a minimal mq.Producer that does not talk to NATS.
type stubMQProducer struct{ closed bool }

func (s *stubMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (s *stubMQProducer) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (s *stubMQProducer) Close() error                                        { s.closed = true; return nil }

func TestInitAudit_DisabledReturnsNilWriter(t *testing.T) {
	cfg := &config.Config{}
	cfg.Audit.Enabled = false
	result, err := InitAudit(cfg, nil, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Writer != nil {
		t.Error("expected nil writer when audit disabled")
	}
}

func TestInitAudit_EnabledNilProducerReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Audit.Enabled = true
	_, err := InitAudit(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error when audit enabled but producer is nil")
	}
}

func TestInitAudit_EnabledWithProducerCreatesWriter(t *testing.T) {
	cfg := &config.Config{}
	cfg.Audit.Enabled = true
	cfg.Audit.Batch.Size = 5
	cfg.Audit.Batch.FlushIntervalMs = 200
	cfg.Audit.Batch.ChannelBufferSize = 50
	producer := &stubMQProducer{}
	result, err := InitAudit(cfg, producer, testLogger())
	if err != nil {
		t.Fatalf("InitAudit: %v", err)
	}
	if result.Writer == nil {
		t.Fatal("expected non-nil writer")
	}
	// Close the writer to avoid goroutine leak.
	if err := result.Writer.Close(context.Background()); err != nil {
		t.Errorf("writer Close: %v", err)
	}
}

func TestInitAudit_EnabledWithDefaultBatchParams(t *testing.T) {
	// Batch params = 0 → code uses defaults (10, 500, 1000).
	cfg := &config.Config{}
	cfg.Audit.Enabled = true
	producer := &stubMQProducer{}
	result, err := InitAudit(cfg, producer, testLogger())
	if err != nil {
		t.Fatalf("InitAudit: %v", err)
	}
	_ = result.Writer.Close(context.Background()) //nolint:errcheck
}

func TestInitAudit_EnabledWithNDJSON_InvalidDir(t *testing.T) {
	// NDJSON dir that doesn't exist → NDJSONWriter init fails → slog.Warn,
	// writer still constructed without fallback.
	cfg := &config.Config{}
	cfg.Audit.Enabled = true
	cfg.Audit.NDJSON.Enabled = true
	cfg.Audit.NDJSON.Dir = "/nonexistent/ndjson-dir-XYZ"
	producer := &stubMQProducer{}
	result, err := InitAudit(cfg, producer, testLogger())
	if err != nil {
		t.Fatalf("InitAudit with bad NDJSON dir: %v", err)
	}
	// Writer still created (NDJSON failure is non-fatal).
	if result.Writer == nil {
		t.Fatal("expected non-nil writer even when NDJSON init fails")
	}
	_ = result.Writer.Close(context.Background()) //nolint:errcheck
}

// Verify stubMQProducer satisfies the mq.Producer interface at compile time.
var _ interface {
	Publish(context.Context, string, []byte) error
	Enqueue(context.Context, string, []byte) error
	Close() error
} = &stubMQProducer{}

// Verify stubAuditWriter (from shutdown_test.go) satisfies sharedaudit.Writer.
var _ sharedaudit.Writer = &stubAuditWriter{}
