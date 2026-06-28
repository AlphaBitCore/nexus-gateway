// proxy_truncation_test.go — a non-streaming upstream response whose
// body was clamped at the read cap (or decompressed-size bound) before usage
// extraction must NOT be billed as a confirmed usage block. Every non-stream
// usage-status path (direct handleNonStream, broker handleNonStreamWithSubscription)
// must stamp usage_extraction_status="truncated" + ResponseTruncated=true so
// downstream cost/analytics treat the token counts as untrusted instead of "ok".
package proxy

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

const okOpenAIBody = `{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

// Direct non-stream path: a truncated ExecutionResult must yield
// usage_extraction_status="truncated" even though the (incomplete) usage
// block parsed to a non-zero token count — proving the handler no longer
// reports "ok" over a clamped buffer.
func TestHandleNonStream_Truncated_StampsTruncatedStatus(t *testing.T) {
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}
	result := &executor.ExecutionResult{
		Body: []byte(okOpenAIBody),
		Usage: provcore.Usage{
			PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
		},
		Truncated: true,
	}

	h.handleNonStream(openAIIngressRequest(t), httptest.NewRecorder(), rec, result,
		target, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	if rec.UsageExtractionStatus != "truncated" {
		t.Errorf("UsageExtractionStatus=%q want truncated", rec.UsageExtractionStatus)
	}
	if !rec.ResponseTruncated {
		t.Error("ResponseTruncated=false want true (truncated body must tag the spilled audit body)")
	}
}

// Control: an identical response that was NOT truncated must still be "ok".
// Pins that the truncated branch fires only on the truncation signal, not on
// every non-stream response (no false positives that would corrupt billing
// in the opposite direction).
func TestHandleNonStream_NotTruncated_StampsOK(t *testing.T) {
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}
	result := &executor.ExecutionResult{
		Body: []byte(okOpenAIBody),
		Usage: provcore.Usage{
			PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
		},
		Truncated: false,
	}

	h.handleNonStream(openAIIngressRequest(t), httptest.NewRecorder(), rec, result,
		target, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	if rec.UsageExtractionStatus != "ok" {
		t.Errorf("UsageExtractionStatus=%q want ok", rec.UsageExtractionStatus)
	}
	if rec.ResponseTruncated {
		t.Error("ResponseTruncated=true want false on a complete body")
	}
}

// Broker non-stream path: the leader fans out a single terminal chunk carrying
// Truncated=true; every joiner draining that chunk must stamp the truncated
// status instead of "ok".
func TestHandleNonStreamWithSubscription_Truncated_StampsTruncatedStatus(t *testing.T) {
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{
			Delta: okOpenAIBody,
			Usage: &provcore.Usage{
				PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
			},
			Truncated: true,
			Done:      true,
		},
	}}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleNonStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		target, nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default(), nil)

	if rec.UsageExtractionStatus != "truncated" {
		t.Errorf("UsageExtractionStatus=%q want truncated", rec.UsageExtractionStatus)
	}
	if !rec.ResponseTruncated {
		t.Error("ResponseTruncated=false want true")
	}
}

// singleChunkSession must propagate the leader's ExecutionResult.Truncated onto
// the terminal chunk so the broker fan-out can replicate the signal.
func TestSingleChunkSession_PropagatesTruncated(t *testing.T) {
	sess := newSingleChunkSession(&executor.ExecutionResult{
		Body:      []byte(okOpenAIBody),
		Truncated: true,
	})
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !c.Truncated {
		t.Error("chunk.Truncated=false want true (leader truncation must ride the terminal chunk)")
	}

	// Negative control: a non-truncated result yields a clean chunk.
	cleanSess := newSingleChunkSession(&executor.ExecutionResult{Body: []byte(okOpenAIBody)})
	clean, err := cleanSess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next (clean): %v", err)
	}
	if clean.Truncated {
		t.Error("chunk.Truncated=true want false on a complete result")
	}
}
