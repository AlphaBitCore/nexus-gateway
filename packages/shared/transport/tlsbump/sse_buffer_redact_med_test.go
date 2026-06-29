package tlsbump

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// fixedModifyHook returns a fixed Modify result (spans + ModifiedContent
// aligned to the test's wire), so a buffer-mode SSE flow drives a real splice
// without standing up the full PII detector + normalize Registry.
type fixedModifyHook struct {
	modified []core.ContentBlock
	spans    []normalize.TransformSpan
}

func (h fixedModifyHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{
		Decision:        core.Modify,
		Reason:          "redact",
		ReasonCode:      "PII",
		ModifiedContent: h.modified,
		TransformSpans:  h.spans,
	}, nil
}

func (fixedModifyHook) SupportsEndpoint(core.EndpointType) bool { return true }
func (fixedModifyHook) SupportsModality(core.Modality) bool     { return true }

func fixedModifyResolver(modified []core.ContentBlock, spans []normalize.TransformSpan) *compliance.PolicyResolver {
	reg := core.NewHookRegistry()
	reg.Register("fixed-modify", func(_ *core.HookConfig) (core.Hook, error) {
		return fixedModifyHook{modified: modified, spans: spans}, nil
	})
	return compliance.NewPolicyResolver([]core.HookConfig{{
		ID:                "h-modify",
		ImplementationID:  "fixed-modify",
		Name:              "fixed-modify",
		Stage:             "response",
		Enabled:           true,
		FailBehavior:      "fail-open",
		ApplicableIngress: []string{"ALL"},
	}}, reg, discardSlog())
}

func sseFrameUpstream(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestSSE_BufferMode_SuccessfulSplice_StampsRedactedBody pins MED-1: on a
// successful buffer-mode splice the audit's ResponseBodyRedacted carries the
// MASKED transcript the client received (so StorageRawBody persists it) instead
// of NULL, and never the original PII.
func TestSSE_BufferMode_SuccessfulSplice_StampsRedactedBody(t *testing.T) {
	modified := []core.ContentBlock{{Type: "text", Text: "ssn [SSN]"}}
	spans := []normalize.TransformSpan{tspan("messages.0.content.0", 4, 15, "[SSN]")}
	store := streampolicy.NewStore(streampolicy.Policy{
		Mode: streampolicy.ModeBufferFullBlock, ChunkBytes: 1024,
		HookTimeoutMs: 1000, MaxBufferBytes: 1 << 20, FailBehavior: streampolicy.FailOpen,
	})
	bo := &bumpOptions{
		policyResolver:       fixedModifyResolver(modified, spans),
		streamingPolicyStore: store,
		auditEmitter:         compliance.NewAuditEmitter(&recordingAuditWriter{}, discardSlog()),
		streamingConfig:      streaming.LiveConfig{MaxBufferSize: 1 << 20},
	}

	respInput := &core.HookInput{Stage: "response", TargetHost: "api.example.com", Path: "/v1/chat/completions", IngressType: "COMPLIANCE_PROXY"}
	auditInfo := &compliance.AuditInfo{TransactionID: "tx-splice"}
	audCtx := &requestAuditCtx{input: respInput, info: *auditInfo, adapter: &openai.Adapter{}, storeResponseBody: true}
	body := `data: {"choices":[{"delta":{"content":"ssn 123-45-6789"}}]}` + "\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	if auditInfo.ResponseBodyRedacted == nil {
		t.Fatal("MED-1: successful splice must stamp ResponseBodyRedacted (not NULL)")
	}
	if strings.Contains(string(auditInfo.ResponseBodyRedacted), "123-45") {
		t.Fatalf("MED-1: redacted audit copy leaked PII: %s", auditInfo.ResponseBodyRedacted)
	}
	if !strings.Contains(string(auditInfo.ResponseBodyRedacted), "[SSN]") {
		t.Fatalf("MED-1: redacted audit copy missing the mask: %s", auditInfo.ResponseBodyRedacted)
	}
	if strings.Contains(rec.Body.String(), "123-45") {
		t.Fatalf("MED-1: client received unredacted PII: %s", rec.Body.String())
	}
}

// TestSSEFrameRedactor_AnthropicRedacts_FencexPasses pins MED-2: an Anthropic
// buffered stream's span-source normalized text and the per-frame
// ExtractStreamChunk wire transcript AGREE — the divergence fence PASSES and the
// splice actually redacts (the disclosed REDACT_INFLIGHT_UNSUPPORTED fail-open
// is NOT taken). Same property is asserted for OpenAI in
// TestSSEFrameRedactor_OpenAIRedacts.
func TestSSEFrameRedactor_AnthropicRedacts_FencePasses(t *testing.T) {
	ctx := context.Background()
	path := "/v1/messages"
	fr := newSSEFrameRedactor(ctx, &anthropic.Adapter{}, path, nil)
	events := []*streaming.SSEEvent{
		anthropicFrame("card "),
		anthropicFrame("4111111111111111"),
		doneFrame(),
	}
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
	}
	if _, err := fr.RedactReplay(events, res); err != nil {
		t.Fatalf("RedactReplay error: %v", err)
	}
	if res.ReasonCode == core.ReasonRedactInflightUnsupported {
		t.Fatal("MED-2: Anthropic fence must PASS (real redaction), not fail-open")
	}
}

// modifyDegradedValue reads the current nexus_streaming_modify_degraded_total
// value for one reason label off the default gatherer.
func modifyDegradedValue(t *testing.T, reason string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "nexus_streaming_modify_degraded_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestSSEFrameRedactor_FailOpenBumpsCounter pins MED-2's observability: a
// splice-divergence fail-open increments nexus_streaming_modify_degraded_total
// under the tlsbump_splice_divergence reason so operators see real redaction
// degrading to forward-original.
func TestSSEFrameRedactor_FailOpenBumpsCounter(t *testing.T) {
	ctx := context.Background()
	fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", nil)
	before := modifyDegradedValue(t, failOpenReasonSplice)

	// ModifiedContent that cannot be reconstructed from the wire (the fence
	// mismatch) forces the splice-divergence fail-open.
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "totally different text"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0", 0, 2, "X")},
	}
	out, err := fr.RedactReplay([]*streaming.SSEEvent{oaFrame("hello"), doneFrame()}, res)
	if err != nil {
		t.Fatalf("fail-open must return nil error, got %v", err)
	}
	if res.ReasonCode != core.ReasonRedactInflightUnsupported {
		t.Fatal("expected the disclosed fail-open reason stamp")
	}
	if serialize(out[0]) != serialize(oaFrame("hello")) {
		t.Fatal("fail-open must forward the original frame unchanged")
	}
	if got := modifyDegradedValue(t, failOpenReasonSplice) - before; got != 1 {
		t.Fatalf("fail-open counter delta = %v, want 1", got)
	}
}

// TestSSEFrameRedactor_ToolArgFailOpenBumpsCounter pins MED-2's observability
// for the tool-arg-undeliverable fail-open reason.
func TestSSEFrameRedactor_ToolArgFailOpenBumpsCounter(t *testing.T) {
	ctx := context.Background()
	fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", nil)
	before := modifyDegradedValue(t, failOpenReasonToolArg)
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "x"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0.toolUse.input.0", 0, 1, "x")},
	}
	if _, err := fr.RedactReplay([]*streaming.SSEEvent{oaFrame("hi"), doneFrame()}, res); err != nil {
		t.Fatalf("fail-open must return nil error, got %v", err)
	}
	if got := modifyDegradedValue(t, failOpenReasonToolArg) - before; got != 1 {
		t.Fatalf("tool-arg fail-open counter delta = %v, want 1", got)
	}
}
