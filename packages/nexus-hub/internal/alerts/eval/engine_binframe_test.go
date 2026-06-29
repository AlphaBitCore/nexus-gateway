package alerteval

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestHandleMQMessage_BinaryTrafficFrameDispatches is the regression for the
// binary-wire bug: the gateway publishes audit on the binary TLV wire by default,
// but the alerts engine used to JSON-decode it (and split it on '\n'), so every
// record failed to decode and threshold alerting was silently dead. handleMQMessage
// must now dual-read the binary frame and dispatch every record. The records carry
// inline bodies to exercise the metadata-only (zero-copy) body decode on this path.
func TestHandleMQMessage_BinaryTrafficFrameDispatches(t *testing.T) {
	e := newEngineForTest(t, &fakeRuleLister{}, &fakeAlertSink{}, &stubMQConsumer{})
	agg := &fireAggregator{id: "rule.x", sources: []EventSource{SourceAITraffic}}
	other := &fireAggregator{id: "rule.y", sources: []EventSource{SourceAgent}}
	e.Register(agg)
	e.Register(other)

	ts := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	msgs := []mq.TrafficEventMessage{
		{ID: "e1", Source: "ai-gateway", Timestamp: ts, LatencyMs: 5,
			RequestBody: audit.NewInlineBody([]byte(`{"prompt":"hi"}`), 15, false, "application/json")},
		{ID: "e2", Source: "ai-gateway", Timestamp: ts, LatencyMs: 6,
			ResponseBody: audit.NewInlineBody([]byte("data: hi\n\n"), 9, false, "text/event-stream")},
	}
	frame := []byte{mq.BinwireMagic}
	for _, m := range msgs {
		rec := m.AppendBinary(nil)
		frame = binary.AppendUvarint(frame, uint64(len(rec)))
		frame = append(frame, rec...)
	}

	if err := e.handleMQMessage(trafficSubjects[SourceAITraffic], &mq.Message{Data: frame}); err != nil {
		t.Fatalf("handleMQMessage: %v", err)
	}
	if agg.eventCount() != 2 {
		t.Errorf("binary frame: ai-traffic aggregator got %d events, want 2 (binary-wire decode regression)", agg.eventCount())
	}
	if other.eventCount() != 0 {
		t.Errorf("agent-source aggregator got %d events, want 0", other.eventCount())
	}
}
