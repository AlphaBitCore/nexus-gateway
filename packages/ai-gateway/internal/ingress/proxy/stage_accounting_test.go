package proxy

import (
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// When pure-forward is on, finalizeAudit must reclaim the pooled request body and
// return WITHOUT enqueuing — the async writer never sees the record, so a capture
// producer receives zero messages, and the pooled body is returned (RequestBody
// cleared) so the benchmark hot path recycles it.
func TestFinalizeAudit_PureForward_SkipsEnqueueAndReclaims(t *testing.T) {
	orig := pureForward
	t.Cleanup(func() { pureForward = orig })
	pureForward = true

	prod := &captureProducer{}
	w := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())
	t.Cleanup(func() { w.Close() })

	body, handle := audit.AcquireRequestBody([]byte(`{"model":"gpt-4o"}`))
	rec := &audit.Record{RequestBody: body}
	rec.AttachPooledRequestBody(handle)

	s := &proxyState{h: &Handler{deps: &Deps{AuditWriter: w}}, rec: rec}
	s.finalizeAudit()

	if rec.RequestBody != nil {
		t.Errorf("pure-forward did not reclaim pooled request body: %v", rec.RequestBody)
	}
	// Pure-forward never calls Enqueue, so the writer's async workers never start
	// and nothing can reach the producer — assert zero captured messages directly.
	prod.mu.Lock()
	n := len(prod.messages)
	prod.mu.Unlock()
	if n != 0 {
		t.Errorf("pure-forward emitted %d audit messages, want 0", n)
	}
	// The early return must not have stamped the latency snapshot.
	if rec.LatencyBreakdown != nil {
		t.Errorf("pure-forward stamped LatencyBreakdown=%v, want nil (early return)", rec.LatencyBreakdown)
	}
}
