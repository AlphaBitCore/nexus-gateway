package wiring

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
)

// TestWireNormalizer_WithMQBatchWriter exercises the type-assertion happy-path
// in WireNormalizer where the writer IS a *audit.MQBatchWriter. We create
// the writer via InitAudit (which is the canonical wiring path) so no
// internal audit package types are needed here.
func TestWireNormalizer_WithRealMQBatchWriter(t *testing.T) {
	cfg := &config.Config{}
	cfg.Audit.Enabled = true
	producer := &stubMQProducer{}
	result, err := InitAudit(cfg, producer, testLogger())
	if err != nil {
		t.Fatalf("InitAudit: %v", err)
	}
	defer result.Writer.Close(context.Background()) //nolint:errcheck

	// WireNormalizer should type-assert to *MQBatchWriter and call
	// WithThingIdentity on it.
	WireNormalizer(result.Writer, "proxy-001", "host.example.com")
}
