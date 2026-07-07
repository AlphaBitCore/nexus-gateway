package tlsbump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

func oaFrame(content string) *streaming.SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
	})
	return &streaming.SSEEvent{Event: "message", Data: string(b), Retry: -1}
}

func anthropicFrame(text string) *streaming.SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return &streaming.SSEEvent{Event: "content_block_delta", Data: string(b), Retry: -1}
}

func doneFrame() *streaming.SSEEvent {
	return &streaming.SSEEvent{Event: "message", Data: "[DONE]", Retry: -1, Done: true}
}

func tspan(addr string, start, end int, repl string) normalize.TransformSpan {
	return normalize.TransformSpan{ContentAddress: addr, Start: start, End: end, Replacement: repl}
}

func wireText(events []*streaming.SSEEvent, codec streaming.WireTextCodec) string {
	var b strings.Builder
	for _, e := range events {
		if t, ok := codec.ChunkText(e.Data); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

func serialize(e *streaming.SSEEvent) string {
	var b bytes.Buffer
	_ = streaming.WriteSSEEvent(&b, e)
	return b.String()
}

// nil adapter → no redactor (buffer keeps its degrade behavior).
func TestNewSSEFrameRedactor_NilAdapter(t *testing.T) {
	if fr := newSSEFrameRedactor(context.Background(), nil, "/v1/chat/completions", false, nil); fr != nil {
		t.Fatalf("expected nil redactor for nil adapter")
	}
}

// Through the REAL OpenAI adapter: a buffered Modify redacts the text frames.
func TestSSEFrameRedactor_OpenAIRedacts(t *testing.T) {
	ctx := context.Background()
	fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", false, nil)
	codec := adapterWireCodec{ctx: ctx, adapter: &openai.Adapter{}, path: "/v1/chat/completions"}

	events := []*streaming.SSEEvent{
		oaFrame("ssn "),
		oaFrame("123-45"),
		oaFrame("-6789"),
		doneFrame(),
	}
	// "ssn 123-45-6789"; PII = [4,15)
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "ssn [SSN]"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0", 4, 15, "[SSN]")},
	}
	out, err := fr.RedactReplay(events, res)
	if err != nil {
		t.Fatalf("RedactReplay error: %v", err)
	}
	if got := wireText(out, codec); got != "ssn [SSN]" {
		t.Fatalf("masked transcript = %q", got)
	}
	for _, e := range out {
		if strings.Contains(e.Data, "123-45") || strings.Contains(e.Data, "6789") {
			t.Fatalf("PII leaked in %s", e.Data)
		}
	}
	if res.ReasonCode == core.ReasonRedactInflightUnsupported {
		t.Fatalf("supported redaction must not stamp the unsupported reason")
	}
}

// Through the REAL Anthropic adapter (NOT a ToolArgMasker): non-OpenAI wire
// redacts, proving the path is not OpenAI-only.
func TestSSEFrameRedactor_AnthropicRedacts(t *testing.T) {
	ctx := context.Background()
	path := "/v1/messages"
	fr := newSSEFrameRedactor(ctx, &anthropic.Adapter{}, path, false, nil)
	codec := adapterWireCodec{ctx: ctx, adapter: &anthropic.Adapter{}, path: path}

	events := []*streaming.SSEEvent{
		anthropicFrame("card "),
		anthropicFrame("4111111111111111"),
		doneFrame(),
	}
	// "card 4111111111111111"; PAN = [5,21)
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
	}
	out, err := fr.RedactReplay(events, res)
	if err != nil {
		t.Fatalf("RedactReplay error: %v", err)
	}
	if got := wireText(out, codec); got != "card [CARD]" {
		t.Fatalf("Anthropic masked transcript = %q", got)
	}
	for _, e := range out {
		if strings.Contains(e.Data, "4111111111111111") {
			t.Fatalf("PAN leaked: %s", e.Data)
		}
	}
}

// Fail-OPEN: a tool-call argument mask is undeliverable on the streaming wire →
// the ORIGINAL events are returned with a nil error and the result is stamped
// REDACT_INFLIGHT_UNSUPPORTED (NOT a fail-closed error, NOT an error frame).
func TestSSEFrameRedactor_ToolArgMaskFailsOpen(t *testing.T) {
	ctx := context.Background()
	// OpenAI IS a ToolArgMasker; the streaming path still cannot deliver tool
	// args through verbatim frames, so it must fail open.
	fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", false, nil)
	events := []*streaming.SSEEvent{oaFrame("hello"), doneFrame()}
	before := serialize(events[0])
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "hello"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0.toolUse.input.0", 0, 1, "x")},
	}
	out, err := fr.RedactReplay(events, res)
	if err != nil {
		t.Fatalf("fail-open must return nil error, got %v", err)
	}
	if res.ReasonCode != core.ReasonRedactInflightUnsupported {
		t.Fatalf("expected REDACT_INFLIGHT_UNSUPPORTED stamp, got %q", res.ReasonCode)
	}
	if serialize(out[0]) != before {
		t.Fatalf("fail-open must forward the original frame unchanged")
	}
}

// Fail-OPEN: a non-ToolArgMasker adapter (Anthropic) with a tool-arg span →
// GuardToolArgMasking reports unsupported → forward original + stamp.
func TestSSEFrameRedactor_NonMaskerToolArgFailsOpen(t *testing.T) {
	ctx := context.Background()
	fr := newSSEFrameRedactor(ctx, &anthropic.Adapter{}, "/v1/messages", false, nil)
	events := []*streaming.SSEEvent{anthropicFrame("hi"), doneFrame()}
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "hi"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0.toolUse.input.0", 0, 1, "x")},
	}
	if _, err := fr.RedactReplay(events, res); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.ReasonCode != core.ReasonRedactInflightUnsupported {
		t.Fatalf("expected unsupported stamp, got %q", res.ReasonCode)
	}
}

// Fail-OPEN: splice cannot reconstruct (fence mismatch) → forward + stamp.
func TestSSEFrameRedactor_SpliceUnsupportedFailsOpen(t *testing.T) {
	ctx := context.Background()
	fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", false, nil)
	events := []*streaming.SSEEvent{oaFrame("hello world"), doneFrame()}
	res := &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "TOTALLY DIFFERENT"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0", 0, 5, "BYE")},
	}
	out, err := fr.RedactReplay(events, res)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.ReasonCode != core.ReasonRedactInflightUnsupported {
		t.Fatalf("expected unsupported stamp, got %q", res.ReasonCode)
	}
	if serialize(out[0]) != serialize(events[0]) {
		t.Fatalf("fail-open must forward original frame")
	}
}

// nil result → no-op, no panic.
func TestSSEFrameRedactor_NilResult(t *testing.T) {
	fr := newSSEFrameRedactor(context.Background(), &openai.Adapter{}, "/v1/chat/completions", false, nil)
	events := []*streaming.SSEEvent{oaFrame("x")}
	out, err := fr.RedactReplay(events, nil)
	if err != nil || len(out) != 1 {
		t.Fatalf("nil result must be a clean no-op")
	}
}

// adapterWireCodec: malformed frame → ok=false (verbatim).
func TestAdapterWireCodec_DecodeError(t *testing.T) {
	codec := adapterWireCodec{ctx: context.Background(), adapter: &openai.Adapter{}, path: "/v1/chat/completions"}
	if _, ok := codec.ChunkText("not json"); ok {
		t.Fatalf("malformed frame must report ok=false")
	}
	if _, ok := codec.ChunkText(`{"choices":[{"delta":{"role":"assistant"}}]}`); ok {
		t.Fatalf("role-only frame must report ok=false")
	}
}

// toolArgSentinel returns non-nil only when a tool-arg leaf is addressed.
func TestToolArgSentinel(t *testing.T) {
	if toolArgSentinel(nil) != nil {
		t.Errorf("nil spans → nil")
	}
	if toolArgSentinel([]normalize.TransformSpan{tspan("messages.0.content.0", 0, 1, "x")}) != nil {
		t.Errorf("text-only spans → nil")
	}
	if got := toolArgSentinel([]normalize.TransformSpan{tspan("messages.0.content.0.toolUse.input.0", 0, 1, "x")}); len(got) == 0 {
		t.Errorf("tool-arg span → non-empty sentinel")
	}
}

// Concurrency: two parallel RedactReplay calls produce distinct masked output
// with no shared state (run under -race).
func TestSSEFrameRedactor_ConcurrentRedactReplay(t *testing.T) {
	ctx := context.Background()
	codec := adapterWireCodec{ctx: ctx, adapter: &openai.Adapter{}, path: "/v1/chat/completions"}
	var wg sync.WaitGroup
	got := make([]string, 6)
	for i := range 6 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			secret := fmt.Sprintf("PII%04d", n)
			events := []*streaming.SSEEvent{
				oaFrame("x "),
				oaFrame(secret),
				oaFrame(" y"),
				doneFrame(),
			}
			res := &core.CompliancePipelineResult{
				Decision:        core.Modify,
				ModifiedContent: []core.ContentBlock{{Type: "text", Text: "x [P] y"}},
				TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0", 2, 2+len(secret), "[P]")},
			}
			fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", false, nil)
			out, err := fr.RedactReplay(events, res)
			if err != nil {
				t.Errorf("goroutine %d error: %v", n, err)
				return
			}
			got[n] = wireText(out, codec)
			if strings.Contains(got[n], secret) {
				t.Errorf("goroutine %d leaked %s", n, secret)
			}
		}(i)
	}
	wg.Wait()
	for n, transcript := range got {
		if transcript != "x [P] y" {
			t.Errorf("goroutine %d transcript = %q", n, transcript)
		}
	}
}

// Posture-follows-caller: an unreconstructable masked wire fails CLOSED for the
// strict (compliance-proxy appliance) caller and fails OPEN for the non-strict
// (agent NE host-packet) caller, across BOTH root causes (tool-arg undeliverable
// and splice divergence). Fail-closed returns streaming.ErrRewriteUnsupported
// with no events (the buffer pipeline then emits the error frame, replaying zero
// original byte); fail-open returns the ORIGINAL events with a nil error. Both
// postures always stamp REDACT_INFLIGHT_UNSUPPORTED for audit honesty.
func TestSSEFrameRedactor_PostureFollowsCaller(t *testing.T) {
	ctx := context.Background()
	// A tool-arg span forces the tool-arg-undeliverable cause; a fence-mismatch
	// ModifiedContent forces the splice-divergence cause.
	toolArgSpans := []normalize.TransformSpan{tspan("messages.0.content.0.toolUse.input.0", 0, 1, "x")}
	spliceSpans := []normalize.TransformSpan{tspan("messages.0.content.0", 0, 5, "BYE")}

	cases := []struct {
		name    string
		strict  bool
		spans   []normalize.TransformSpan
		modText string
	}{
		{"toolArg/strict_fail_closed", true, toolArgSpans, "hello"},
		{"toolArg/nonstrict_fail_open", false, toolArgSpans, "hello"},
		{"splice/strict_fail_closed", true, spliceSpans, "TOTALLY DIFFERENT"},
		{"splice/nonstrict_fail_open", false, spliceSpans, "TOTALLY DIFFERENT"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", tc.strict, nil)
			events := []*streaming.SSEEvent{oaFrame("hello world"), doneFrame()}
			before := serialize(events[0])
			res := &core.CompliancePipelineResult{
				Decision:        core.Modify,
				ModifiedContent: []core.ContentBlock{{Type: "text", Text: tc.modText}},
				TransformSpans:  tc.spans,
			}

			out, err := fr.RedactReplay(events, res)

			// Audit honesty: both postures stamp the unsupported reason.
			if res.ReasonCode != core.ReasonRedactInflightUnsupported {
				t.Fatalf("expected REDACT_INFLIGHT_UNSUPPORTED stamp, got %q", res.ReasonCode)
			}

			if tc.strict {
				// FAIL-CLOSED: ErrRewriteUnsupported, no events. The buffer
				// pipeline turns this into an error frame with zero original
				// replay — no unredacted byte can reach the client.
				if !errors.Is(err, streaming.ErrRewriteUnsupported) {
					t.Fatalf("strict caller must return ErrRewriteUnsupported, got %v", err)
				}
				if out != nil {
					t.Fatalf("strict fail-closed must return no events (no original forwarded), got %d", len(out))
				}
				return
			}

			// FAIL-OPEN: nil error, ORIGINAL events forwarded unchanged.
			if err != nil {
				t.Fatalf("non-strict caller must fail open with nil error, got %v", err)
			}
			if len(out) == 0 || serialize(out[0]) != before {
				t.Fatalf("fail-open must forward the original frame unchanged")
			}
		})
	}
}

// Strict fail-closed end-to-end through the buffer pipeline: a strict redactor
// whose splice cannot reconstruct makes BufferPipeline.Process emit the policy
// error frame and replay ZERO original byte — the client never sees the
// unredacted PII. This pins the leak-prevention contract that the strict
// posture exists for.
func TestSSEFrameRedactor_StrictFailClosedThroughBuffer(t *testing.T) {
	ctx := context.Background()
	pii := "ssn 123-45-6789"
	exec := redactExecutor{result: &core.CompliancePipelineResult{
		Decision: core.Modify,
		// Fence mismatch: ModifiedContent text does not match the wire
		// transcript, forcing splice divergence → unreconstructable (same shape
		// the splice fail-open unit test uses, here driven end-to-end).
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: "TOTALLY DIFFERENT"}},
		TransformSpans:  []normalize.TransformSpan{tspan("messages.0.content.0", 0, 5, "BYE")},
	}}
	fr := newSSEFrameRedactor(ctx, &openai.Adapter{}, "/v1/chat/completions", true, nil)

	pipeline := streaming.NewBufferPipeline(
		streaming.BufferConfig{MaxBufferSize: 1 << 20}, exec, nil,
	).WithFrameRedactor(fr)

	body := strings.NewReader(
		"data: " + oaFrame(pii).Data + "\n\n" +
			"data: [DONE]\n\n",
	)
	var client bytes.Buffer
	res, err := pipeline.Process(ctx, body, &client, &core.HookInput{Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("buffer pipeline error: %v", err)
	}
	if res.ReasonCode != core.ReasonRedactInflightUnsupported {
		t.Fatalf("strict fail-closed must stamp REDACT_INFLIGHT_UNSUPPORTED, got %q", res.ReasonCode)
	}
	if strings.Contains(client.String(), "123-45") || strings.Contains(client.String(), "6789") {
		t.Fatalf("strict fail-closed leaked unredacted PII to client: %q", client.String())
	}
	if !strings.Contains(client.String(), "error") {
		t.Fatalf("strict fail-closed must emit an error frame, got %q", client.String())
	}
}

// redactExecutor is a streaming.PipelineExecutor test double that returns a
// fixed Modify result so the buffer pipeline routes into the frame redactor.
type redactExecutor struct {
	result *core.CompliancePipelineResult
}

func (e redactExecutor) Execute(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
	return e.result
}

// Guard: the OpenAI adapter satisfies traffic.ToolArgMasker (anchors the
// tool-arg fail-open reasoning).
func TestOpenAIIsToolArgMasker(t *testing.T) {
	if !traffic.ToolArgMaskingSupported(&openai.Adapter{}) {
		t.Fatalf("openai adapter must be a ToolArgMasker")
	}
	if traffic.ToolArgMaskingSupported(&anthropic.Adapter{}) {
		t.Fatalf("anthropic adapter must NOT be a ToolArgMasker")
	}
}
