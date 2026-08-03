package audit

import (
	"context"
	"strings"
	"testing"
	"time"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// Per-audit-event marshal cost on the compliance-proxy publish path.
//
// Cost unit: ONE bumped request. flushBatch marshals and publishes exactly one
// TrafficEventMessage per AuditEvent, and the proxy emits one AuditEvent per bumped
// request, so an allocation counted here is an allocation per intercepted request —
// it is merely deferred onto the batch goroutine.
//
// The arms sweep captured-body size because the message is dominated by
// RequestBody/ResponseBody when payload capture is on, and carries only the envelope
// when it is off. Those two regimes have very different answers, so both are measured
// rather than one being assumed representative.

// benchNullProducer is the cheapest Producer that satisfies the publish contract:
// it takes the bytes and returns, retaining nothing. Retention is what a pooled
// marshal buffer would break, so a benchmark producer that appended `data` to a
// slice would both measure a copy production does not pay and hide the hazard.
type benchNullProducer struct{ sink int }

func (p *benchNullProducer) Publish(_ context.Context, _ string, data []byte) error {
	p.sink += len(data)
	return nil
}

func (p *benchNullProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	p.sink += len(data)
	return nil
}
func (p *benchNullProducer) Close() error { return nil }

// benchEvent builds one AuditEvent with a captured body of bodyBytes in each
// direction. The envelope fields mirror what forward.Run actually stamps, so the
// marshal walks a production-shaped struct rather than a mostly-zero one.
func benchEvent(bodyBytes int) AuditEvent {
	status := 200
	reason := "no match"
	code := "clean"
	ua := "curl/8.7.1"
	e := AuditEvent{
		ID:                    "018f5b1c-0000-7000-8000-0123456789ab",
		TransactionID:         "018f5b1c-0000-7000-8000-0123456789ac",
		ConnectionID:          "203.0.113.7:54321->api.openai.com:443",
		TraceID:               "018f5b1c-0000-7000-8000-0123456789ad",
		TrafficSource:         "COMPLIANCE_PROXY",
		IngressType:           "CONNECT",
		BumpStatus:            "BUMP_SUCCESS",
		SourceIP:              "203.0.113.7",
		TargetHost:            "api.openai.com",
		Method:                "POST",
		Path:                  "/v1/chat/completions",
		RequestHookDecision:   "APPROVE",
		RequestHookReason:     &reason,
		RequestHookReasonCode: &code,
		StatusCode:            &status,
		UserAgent:             &ua,
		LatencyMs:             412,
		Timestamp:             time.Unix(1750000000, 0).UTC(),
		Provider:              "openai",
		Model:                 "gpt-4o",
		PromptTokens:          900,
		CompletionTokens:      700,
		TotalTokens:           1600,
		IngressFormat:         "openai-compat",
		ComplianceTags:        []string{"pii-scan", "dlp"},
		RequestHooksPipeline:  []byte(`[{"hookId":"h-1","decision":"APPROVE"}]`),
	}
	if bodyBytes > 0 {
		req := `{"model":"gpt-4o","messages":[{"role":"user","content":"` +
			strings.Repeat("summarize this paragraph ", bodyBytes/25) + `"}]}`
		resp := `{"id":"c-1","choices":[{"message":{"role":"assistant","content":"` +
			strings.Repeat("here is the summary ", bodyBytes/20) + `"}}]}`
		e.RequestBody = sharedaudit.NewInlineBody([]byte(req), int64(len(req)), false, "application/json")
		e.ResponseBody = sharedaudit.NewInlineBody([]byte(resp), int64(len(resp)), false, "application/json")
	}
	return e
}

func benchFlushBatch(b *testing.B, bodyBytes int) {
	b.Helper()
	w := &MQBatchWriter{producer: &benchNullProducer{}, queue: "q", logger: quietLogger()}
	events := []AuditEvent{benchEvent(bodyBytes)}
	ctx := context.Background()
	b.ReportAllocs()
	for range b.N {
		if err := w.flushBatch(ctx, events); err != nil {
			b.Fatalf("flushBatch: %v", err)
		}
	}
}

// BenchmarkFlushBatch_NoBody is the payload-capture-OFF regime: envelope only.
func BenchmarkFlushBatch_NoBody(b *testing.B) { benchFlushBatch(b, 0) }

// BenchmarkFlushBatch_8KiBBodies is the payload-capture-ON regime at a chat-sized
// request/response pair, which is what an intercepted LLM call actually carries.
func BenchmarkFlushBatch_8KiBBodies(b *testing.B) { benchFlushBatch(b, 8<<10) }

// BenchmarkFlushBatch_64KiBBodies is the large-prompt tail of the same regime.
func BenchmarkFlushBatch_64KiBBodies(b *testing.B) { benchFlushBatch(b, 64<<10) }
