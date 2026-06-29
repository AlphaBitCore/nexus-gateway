package wiring

import (
	"context"
	"testing"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// WireNormalizer accepts an audit.Writer (which is an interface alias for
// sharedaudit.Writer). When passed a non-*MQBatchWriter it silently skips the
// identity wiring but must not panic and must build the normalize registry.

// plainWriter is a simple audit.Writer that is NOT a *MQBatchWriter.
type plainWriter struct{}

func (p *plainWriter) Enqueue(_ sharedaudit.AuditEvent) {}
func (p *plainWriter) Flush(_ context.Context) error    { return nil }
func (p *plainWriter) Close(_ context.Context) error    { return nil }

func TestWireNormalizer_NonMQWriterDoesNotPanic(t *testing.T) {
	// A writer that does not implement *audit.MQBatchWriter — the type assertion
	// in WireNormalizer must fall through cleanly.
	WireNormalizer(&plainWriter{}, "proxy-id", "hostname")
}

func TestWireNormalizer_NilWriterDoesNotPanic(t *testing.T) {
	// nil writer — the type assertion must not panic on a nil interface.
	WireNormalizer(nil, "proxy-id", "hostname")
}
