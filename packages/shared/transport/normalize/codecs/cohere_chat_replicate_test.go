package codecs

// Coverage-gap tests pinning observable behavior across the normalize package.
// Each test asserts a named failure mode or canonical output shape; tests do
// not pad coverage with bare err==nil assertions. Organised by source file:
//
//   - cohere_chat.go        — request/response/stream/error paths
//   - replicate.go          — request/response/output polymorphism/usage
//   - projection.go         — AI + HTTP text projection, JoinedText, redact bypass
//   - metrics.go            — Prometheus registration + cache
//   - auditbridge.go        — core.BuildAuditFn nil-reg + marshal-error + core.StripContentTypeParams + core.MustRegisterPrometheus
//   - registry.go           — Replace + All + SetConfidenceThreshold + RegisterTier2 + core.MaybeGunzip(zlib+zstd+truncated) + Normalize tiers
//   - apply_spans.go        — form map mutation via mapEntryRef + core.ClonePayload variants + resolveTextRef edges
//   - types.go              — IsHTTP, IsAI gaps, Direction constants, MarshalJSON nil-Content
//   - anthropic_messages.go — MergeAnthropicEventUsage + anthropicContentPart variants + system-empty
//   - openai_chat.go        — extractCanonicalUsage every alias chain + roleFromString unknown + indexOfToolCall
//   - gemini_generate.go    — ExtractGeminiEventUsage + role coverage + inlineData/functionCall on response side
//   - openai_responses.go   — ID() + tool_call alternative + input_image + arguments-only fallback
//   - generic_http.go       — splitMediaTypeAndParams unparseable + looksLikeText positive arms + form decode error + multipart partial

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/klauspost/compress/zstd"
	"github.com/prometheus/client_golang/prometheus"
)

func TestCohereChat_ID(t *testing.T) {
	if id := NewCohereChatNormalizer().ID(); id != "cohere-chat" {
		t.Fatalf("ID = %q", id)
	}
}

func TestCohereChat_RequestStringContent(t *testing.T) {
	body := `{"model":"command-r","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}],"stream":true}`
	got, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "cohere-chat" {
		t.Fatalf("Protocol = %q", got.Protocol)
	}
	if got.Model != "command-r" {
		t.Fatalf("Model = %q", got.Model)
	}
	if !got.Stream {
		t.Fatalf("Stream not set")
	}
	if len(got.Messages) != 2 ||
		got.Messages[0].Content[0].Text != "hi" ||
		got.Messages[1].Content[0].Text != "hello" ||
		got.Messages[0].Role != core.RoleUser ||
		got.Messages[1].Role != core.RoleAssistant {
		t.Fatalf("messages wrong: %+v", got.Messages)
	}
	if got.DetectedSpec != "cohere-chat" {
		t.Fatalf("DetectedSpec = %q", got.DetectedSpec)
	}
}

func TestCohereChat_RequestUnknownRoleFallback(t *testing.T) {
	// Non-string content falls through to string(raw) per code comments;
	// the helper preserves the raw bytes verbatim.
	body := `{"model":"command","messages":[{"role":"user","content":["multi","part"]}]}`
	got, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Messages[0].Content[0].Text == "" {
		t.Fatalf("expected raw JSON preserved as text fallback; got empty")
	}
	if !strings.Contains(got.Messages[0].Content[0].Text, "multi") {
		t.Fatalf("raw-fallback lost payload: %q", got.Messages[0].Content[0].Text)
	}
}

func TestCohereChat_RequestEmptyMessages(t *testing.T) {
	body := `{"model":"command","messages":[]}`
	_, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestCohereChat_RequestMalformed(t *testing.T) {
	_, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte("not-json"), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if errors.Is(err, core.ErrUnsupported) {
		t.Fatal("malformed JSON should not be core.ErrUnsupported")
	}
}

func TestCohereChat_ResponseWithToolPlan(t *testing.T) {
	body := `{"id":"r1","model":"command","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"Hi"},{"type":"text","text":"!"}],"tool_plan":"think first"},"usage":{"tokens":{"input_tokens":10,"output_tokens":3}}}`
	got, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.FinishReason != "COMPLETE" {
		t.Fatalf("FinishReason = %q", got.FinishReason)
	}
	// Reasoning block (tool_plan) comes BEFORE text per code order.
	blocks := got.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (reasoning + concatenated text), got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != core.ContentReasoning || blocks[0].Text != "think first" {
		t.Fatalf("reasoning block wrong: %+v", blocks[0])
	}
	if blocks[1].Type != core.ContentText || blocks[1].Text != "Hi!" {
		t.Fatalf("text block wrong: %+v", blocks[1])
	}
	if got.Usage == nil || got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 10 || *got.Usage.CompletionTokens != 3 || *got.Usage.TotalTokens != 13 {
		t.Fatalf("usage wrong: %+v", got.Usage)
	}
}

func TestCohereChat_ResponseBilledUnitsFallback(t *testing.T) {
	// usage.tokens missing → fall back to billed_units.
	body := `{"id":"r","model":"x","finish_reason":"DONE","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},"usage":{"billed_units":{"input_tokens":7,"output_tokens":2}}}`
	got, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Usage == nil || *got.Usage.PromptTokens != 7 || *got.Usage.CompletionTokens != 2 || *got.Usage.TotalTokens != 9 {
		t.Fatalf("billed_units fallback wrong: %+v", got.Usage)
	}
}

func TestCohereChat_ResponseUsageZero(t *testing.T) {
	// Both blocks at zero → cohereUsageToCanonical returns nil.
	body := `{"id":"r","model":"x","finish_reason":"DONE","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},"usage":{"tokens":{"input_tokens":0,"output_tokens":0}}}`
	got, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Usage != nil {
		t.Fatalf("expected nil usage when both counts zero, got %+v", got.Usage)
	}
}

func TestCohereChat_ResponseNoUsage(t *testing.T) {
	body := `{"id":"r","model":"x","finish_reason":"DONE","message":{"role":"assistant","content":[]}}`
	got, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Usage != nil {
		t.Fatalf("expected nil core.Usage when no usage block, got %+v", got.Usage)
	}
}

func TestCohereChat_ResponseUsageBothBlocksAbsent(t *testing.T) {
	// usage object present but neither tokens nor billed_units → nil.
	c := cohereUsage{}
	if got := cohereUsageToCanonical(&c); got != nil {
		t.Fatalf("expected nil when usage has no fields, got %+v", got)
	}
}

func TestCohereChat_ResponseMissingMessage(t *testing.T) {
	body := `{"id":"r","model":"x","finish_reason":"DONE","usage":{"tokens":{"input_tokens":1,"output_tokens":1}}}`
	got, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported when message missing, got %v", err)
	}
	// Usage was still extracted first.
	if got.Usage == nil || *got.Usage.PromptTokens != 1 {
		t.Fatalf("usage should be extracted before message check: %+v", got.Usage)
	}
}

func TestCohereChat_ResponseMalformed(t *testing.T) {
	_, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte("xxx"), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected unmarshal err")
	}
	if errors.Is(err, core.ErrUnsupported) {
		t.Fatal("malformed JSON should not be core.ErrUnsupported")
	}
}

func TestCohereChat_EmptyBody(t *testing.T) {
	for _, d := range []core.Direction{core.DirectionRequest, core.DirectionResponse, core.Direction("???")} {
		_, err := NewCohereChatNormalizer().Normalize(context.Background(), nil, core.Meta{Direction: d})
		if !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("dir %v: expected core.ErrUnsupported, got %v", d, err)
		}
	}
}

func TestCohereChat_UnknownDirection(t *testing.T) {
	_, err := NewCohereChatNormalizer().Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: core.Direction("weird")})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported on unknown direction, got %v", err)
	}
}

func TestCohereChat_FieldSpec(t *testing.T) {
	req := cohereChatFieldSpec(core.DirectionRequest)
	if len(req.Required) == 0 || req.Required[0] != "model" {
		t.Fatalf("request required wrong: %+v", req.Required)
	}
	resp := cohereChatFieldSpec(core.DirectionResponse)
	if len(resp.Required) == 0 || resp.Required[0] != "id" {
		t.Fatalf("response required wrong: %+v", resp.Required)
	}
}

func TestReplicate_ID(t *testing.T) {
	if id := NewReplicateNormalizer().ID(); id != "replicate-prediction" {
		t.Fatalf("ID = %q", id)
	}
}

func TestReplicate_RequestWithPrompt(t *testing.T) {
	body := `{"version":"meta/llama","stream":true,"input":{"prompt":"Why?"}}`
	got, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Model != "meta/llama" {
		t.Fatalf("Model = %q", got.Model)
	}
	if !got.Stream {
		t.Fatalf("Stream not propagated")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != core.RoleUser || got.Messages[0].Content[0].Text != "Why?" {
		t.Fatalf("messages wrong: %+v", got.Messages)
	}
}

func TestReplicate_RequestWithMessagesArray(t *testing.T) {
	body := `{"version":"x","input":{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}}`
	got, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 2 || got.Messages[1].Role != core.RoleAssistant {
		t.Fatalf("messages wrong: %+v", got.Messages)
	}
}

func TestReplicate_RequestNoRecoverableContent(t *testing.T) {
	// input present, parseable, but neither prompt nor messages set.
	body := `{"version":"x","input":{"unknown":"value"}}`
	_, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestReplicate_RequestEmptyInput(t *testing.T) {
	// input absent / null.
	body := `{"version":"x","input":null}`
	_, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestReplicate_RequestUnparseableInput(t *testing.T) {
	// input is valid JSON but not a recognisable object — string passes
	// json.Unmarshal into replicateInput with empty fields, no content.
	body := `{"version":"x","input":"raw-string"}`
	_, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestReplicate_RequestMalformed(t *testing.T) {
	_, err := NewReplicateNormalizer().Normalize(context.Background(), []byte("xx"), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if errors.Is(err, core.ErrUnsupported) {
		t.Fatal("malformed should not be core.ErrUnsupported")
	}
}

func TestReplicate_ResponseOutputString(t *testing.T) {
	body := `{"id":"p","version":"m","status":"succeeded","output":"hello world","metrics":{"input_token_count":5,"output_token_count":2}}`
	got, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Messages[0].Content[0].Text != "hello world" {
		t.Fatalf("output text wrong: %q", got.Messages[0].Content[0].Text)
	}
	if got.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q", got.FinishReason)
	}
	if got.Usage == nil || *got.Usage.PromptTokens != 5 || *got.Usage.CompletionTokens != 2 || *got.Usage.TotalTokens != 7 {
		t.Fatalf("usage wrong: %+v", got.Usage)
	}
}

func TestReplicate_ResponseOutputArray(t *testing.T) {
	body := `{"id":"p","version":"m","status":"succeeded","output":["one","two","three"]}`
	got, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Messages[0].Content[0].Text != "onetwothree" {
		t.Fatalf("array concat wrong: %q", got.Messages[0].Content[0].Text)
	}
}

func TestReplicate_ResponseOutputObjectKeys(t *testing.T) {
	cases := []struct {
		key, want string
	}{
		{"text", "via-text"},
		{"answer", "via-answer"},
		{"completion", "via-completion"},
		{"message", "via-message"},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			body := `{"id":"p","version":"m","status":"succeeded","output":{"` + c.key + `":"` + c.want + `"}}`
			got, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Messages[0].Content[0].Text != c.want {
				t.Fatalf("object-key %q: got %q want %q", c.key, got.Messages[0].Content[0].Text, c.want)
			}
		})
	}
}

func TestReplicate_ResponseFailedWithError(t *testing.T) {
	body := `{"id":"p","status":"failed","output":null,"error":"upstream blew up"}`
	got, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.FinishReason != "error" {
		t.Fatalf("FinishReason = %q want error", got.FinishReason)
	}
	if got.Messages[0].Content[0].Text != "upstream blew up" {
		t.Fatalf("error msg should become the assistant content: %q", got.Messages[0].Content[0].Text)
	}
}

func TestReplicate_ResponseFailedNoError(t *testing.T) {
	// failed status + no error string + no output → core.ErrUnsupported.
	body := `{"id":"p","status":"failed","output":null}`
	_, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestReplicate_ResponseCanceledStatus(t *testing.T) {
	body := `{"id":"p","status":"canceled","output":"partial"}`
	got, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.FinishReason != "error" {
		t.Fatalf("FinishReason for canceled = %q", got.FinishReason)
	}
}

func TestReplicate_ResponseMalformed(t *testing.T) {
	_, err := NewReplicateNormalizer().Normalize(context.Background(), []byte("nope"), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestReplicate_EmptyAndUnknownDirection(t *testing.T) {
	n := NewReplicateNormalizer()
	if _, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionRequest}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("empty body: expected core.ErrUnsupported, got %v", err)
	}
	if _, err := n.Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: core.Direction("weird")}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("weird direction: expected core.ErrUnsupported, got %v", err)
	}
}

func TestReplicate_ExtractOutputTextEdgeCases(t *testing.T) {
	// empty / null / unknown.
	if got := replicateExtractOutputText(nil); got != "" {
		t.Fatalf("nil should be empty, got %q", got)
	}
	if got := replicateExtractOutputText([]byte("null")); got != "" {
		t.Fatalf("null should be empty, got %q", got)
	}
	if got := replicateExtractOutputText([]byte(`{"unknown":"key"}`)); got != "" {
		t.Fatalf("unknown object should be empty, got %q", got)
	}
	if got := replicateExtractOutputText([]byte(`123`)); got != "" {
		t.Fatalf("number-only should be empty, got %q", got)
	}
}

func TestReplicate_FieldSpec(t *testing.T) {
	r := replicateFieldSpec(core.DirectionRequest)
	if len(r.Required) == 0 || r.Required[0] != "input" {
		t.Fatalf("request required wrong: %+v", r.Required)
	}
	r = replicateFieldSpec(core.DirectionResponse)
	if len(r.Required) == 0 || r.Required[0] != "id" {
		t.Fatalf("response required wrong: %+v", r.Required)
	}
}

func TestTextProjection_NilPayload(t *testing.T) {
	var p *core.NormalizedPayload
	if got := p.TextProjection(); got != nil {
		t.Fatalf("nil payload should yield nil; got %v", got)
	}
}

func TestTextProjection_RedactedShortCircuits(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind:     core.KindAIChat,
		Redacted: true,
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "hidden"}}}},
	}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("redacted payload must hide content; got %v", got)
	}
}

func TestTextProjection_AIBlocksAndToolResults(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindAIChat,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: []core.ContentBlock{{Type: core.ContentText, Text: "system"}}},
			{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: ""}, {Type: core.ContentText, Text: "user-text"}}},
			{Role: core.RoleAssistant, Content: []core.ContentBlock{
				{Type: core.ContentReasoning, Text: "thought (skipped by default)"},
				{Type: core.ContentToolUse, ToolUse: &core.ToolUse{Name: "x"}}, // unaccounted type
				{Type: core.ContentToolResult, ToolResult: &core.ToolResult{Output: "tool-out"}},
				{Type: core.ContentToolResult, ToolResult: nil},                          // nil tool result skipped
				{Type: core.ContentToolResult, ToolResult: &core.ToolResult{Output: ""}}, // empty skipped
			}},
		},
	}
	got := p.TextProjection()
	want := []string{"system", "user-text", "tool-out"}
	if len(got) != len(want) {
		t.Fatalf("len = %d (%v) want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

func TestTextProjectionWith_IncludeReasoning(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindAIChat,
		Messages: []core.Message{
			{Role: core.RoleAssistant, Content: []core.ContentBlock{
				{Type: core.ContentReasoning, Text: "let me think"},
				{Type: core.ContentReasoning, Text: ""}, // empty reasoning skipped
				{Type: core.ContentText, Text: "answer"},
			}},
		},
	}
	got := p.TextProjectionWith(core.TextProjectionOptions{IncludeReasoning: true})
	want := []string{"let me think", "answer"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("opt-in reasoning = %v want %v", got, want)
	}
}

func TestTextProjection_HTTPText(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindHTTPText,
		HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{Text: "raw body"}},
	}
	got := p.TextProjection()
	if len(got) != 1 || got[0] != "raw body" {
		t.Fatalf("got %v", got)
	}
}

func TestTextProjection_HTTPForm(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindHTTPForm,
		HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{Form: map[string]string{"k1": "v1", "k2": "v2"}}},
	}
	got := p.TextProjection()
	if len(got) != 2 {
		t.Fatalf("len = %d want 2 (%v)", len(got), got)
	}
	// Map iteration is unordered; verify each entry shape.
	for _, entry := range got {
		if !strings.Contains(entry, "=") {
			t.Fatalf("entry should be key=value: %q", entry)
		}
	}
}

func TestTextProjection_HTTPNilBodyView(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindHTTPText,
		HTTP: &core.HTTPPayload{BodyView: nil},
	}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("nil body view should yield nil; got %v", got)
	}
}

func TestTextProjection_HTTPNil(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindHTTPText,
		HTTP: nil,
	}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("nil HTTP should yield nil; got %v", got)
	}
}

func TestTextProjection_HTTPEmpty(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindHTTPText,
		HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{}}, // no Text, no Form
	}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("expected nil for empty body view; got %v", got)
	}
}

func TestTextProjection_UnsupportedKindReturnsNil(t *testing.T) {
	p := &core.NormalizedPayload{Kind: core.KindUnsupported}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestJoinedText(t *testing.T) {
	p := &core.NormalizedPayload{
		Kind: core.KindAIChat,
		Messages: []core.Message{
			{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "a"}, {Type: core.ContentText, Text: "b"}}},
		},
	}
	if got := p.JoinedText(" | "); got != "a | b" {
		t.Fatalf("JoinedText = %q want %q", got, "a | b")
	}
	empty := &core.NormalizedPayload{Kind: core.KindAIChat}
	if got := empty.JoinedText("|"); got != "" {
		t.Fatalf("empty payload should produce empty join; got %q", got)
	}
}

func TestNewMetrics_RegistersAndCaches(t *testing.T) {
	// Fresh namespace registers; second call returns cached instance.
	reg := prometheus.NewRegistry()
	ns := "test_normalize_metrics_unique_a"
	m1 := core.NewMetrics(reg, ns)
	if m1 == nil || m1.Total == nil || m1.LatencyMs == nil || m1.PayloadBytes == nil || m1.FallbackTotal == nil {
		t.Fatalf("metric fields nil: %+v", m1)
	}
	m2 := core.NewMetrics(reg, ns)
	if m1 != m2 {
		t.Fatalf("second call should return cached instance, got distinct pointers")
	}
	// Sanity exercise: incrementing counters must not panic.
	m1.Total.WithLabelValues("a", "b", "c", "d").Inc()
	m1.LatencyMs.WithLabelValues("a", "b").Observe(1.0)
	m1.PayloadBytes.WithLabelValues("a", "b").Observe(1.0)
	m1.FallbackTotal.WithLabelValues("r").Inc()
}

func TestMustRegisterPrometheus_NilReturnsNil(t *testing.T) {
	if got := core.MustRegisterPrometheus(nil, "x"); got != nil {
		t.Fatalf("nil reg → expected nil metrics, got %+v", got)
	}
}

func TestMustRegisterPrometheus_RegistersWhenNonNil(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := core.MustRegisterPrometheus(reg, "test_normalize_metrics_unique_b")
	if m == nil {
		t.Fatal("expected non-nil metrics when reg non-nil")
	}
}

func TestBuildAuditFn_NilRegistryReturnsNil(t *testing.T) {
	if got := core.BuildAuditFn(nil, nil); got != nil {
		t.Fatalf("nil registry → expected nil fn, got %T", got)
	}
}

func TestBuildAuditFn_EmptyBodyShortCircuits(t *testing.T) {
	reg := core.NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()
	fn := core.BuildAuditFn(reg, nil)
	raw, status, reason := fn("request", "application/json", "openai", "x", "/v1/chat/completions", false, nil)
	if raw != nil || status != "" || reason != "" {
		t.Fatalf("empty body should produce zero values; got raw=%v status=%q reason=%q", raw, status, reason)
	}
}

func TestBuildAuditFn_FailedSurfaceWithMetrics(t *testing.T) {
	reg := core.NewRegistry()
	reg.Freeze() // no normalizers → guaranteed core.ErrUnsupported
	pReg := prometheus.NewRegistry()
	m := core.NewMetrics(pReg, "test_normalize_metrics_unique_c")
	fn := core.BuildAuditFn(reg, m)
	raw, status, reason := fn("request", "application/json", "novendor", "x", "/v1/x", false, []byte(`{}`))
	if status != "failed" {
		t.Fatalf("status = %q want failed", status)
	}
	if reason == "" {
		t.Fatalf("reason should be populated on failed")
	}
	if raw != nil {
		t.Fatalf("raw should be nil on failed; got %s", raw)
	}
}

func TestBuildAuditFn_PartialStatus(t *testing.T) {
	// Stub normalizer returning a non-core.ErrUnsupported error + a payload.
	reg := core.NewRegistry()
	reg.Register("custom", &stubNormalizer{id: "custom", payload: core.NormalizedPayload{Kind: core.KindAIChat, Protocol: "custom"}, err: errors.New("partial parse")})
	reg.Freeze()
	pReg := prometheus.NewRegistry()
	m := core.NewMetrics(pReg, "test_normalize_metrics_unique_d")
	fn := core.BuildAuditFn(reg, m)
	raw, status, reason := fn("response", "application/json", "custom", "x", "/v1/x", false, []byte(`{}`))
	if status != "partial" {
		t.Fatalf("status = %q want partial", status)
	}
	if reason == "" {
		t.Fatalf("reason should describe the error")
	}
	if len(raw) == 0 {
		t.Fatalf("partial should still marshal payload; got empty")
	}
}

func TestBuildAuditFn_OkStatusAndDefaults(t *testing.T) {
	// Stub returns no error, no Protocol/Kind → audit must default them to "unsupported".
	reg := core.NewRegistry()
	reg.Register("custom2", &stubNormalizer{id: "custom2"}) // zero payload, no err
	reg.Freeze()
	fn := core.BuildAuditFn(reg, nil)
	raw, status, reason := fn("response", "application/json", "custom2", "x", "/v1/x", false, []byte(`{}`))
	if status != "ok" {
		t.Fatalf("status = %q want ok", status)
	}
	if reason != "" {
		t.Fatalf("reason should be empty on ok; got %q", reason)
	}
	if len(raw) == 0 {
		t.Fatal("raw must be populated on ok")
	}
}

func TestStripContentTypeParams(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"application/json":                "application/json",
		"application/json; charset=utf-8": "application/json",
		" text/html ;x=y":                 "text/html",
		"application/x-custom; key=v ":    "application/x-custom",
	}
	for in, want := range cases {
		if got := core.StripContentTypeParams(in); got != want {
			t.Errorf("core.StripContentTypeParams(%q) = %q want %q", in, got, want)
		}
	}
}

func TestRegistry_AllReturnsKeys(t *testing.T) {
	r := core.NewRegistry()
	r.Register("a", &stubNormalizer{id: "a"})
	r.Register("b", &stubNormalizer{id: "b"})
	r.Freeze()
	got := r.All()
	if len(got) != 2 {
		t.Fatalf("All() = %v want 2 keys", got)
	}
}

func TestRegistry_ReplaceOverwrites(t *testing.T) {
	r := core.NewRegistry()
	first := &stubNormalizer{id: "first"}
	r.Register("k", first)
	second := &stubNormalizer{id: "second"}
	r.Replace("k", second)
	if got := r.Resolve(core.Meta{AdapterType: "k"}); got != second {
		t.Fatalf("Replace did not overwrite")
	}
}

func TestRegistry_ReplaceOnEmptyKey(t *testing.T) {
	// Replace allowed for a key that was never registered.
	r := core.NewRegistry()
	stub := &stubNormalizer{id: "z"}
	r.Replace("k", stub)
	if got := r.Resolve(core.Meta{AdapterType: "k"}); got != stub {
		t.Fatalf("Replace into empty registry failed")
	}
}

func TestRegistry_ReplacePanicsOnFrozen(t *testing.T) {
	r := core.NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.Replace("k", &stubNormalizer{})
}

func TestRegistry_SetConfidenceThreshold_Clamps(t *testing.T) {
	r := core.NewRegistry()
	r.SetConfidenceThreshold(-1)
	r.SetConfidenceThreshold(2)
	// no observable getter — exercise that no panic + still works.
	r.Register("k", &stubNormalizer{id: "k", payload: core.NormalizedPayload{Kind: core.KindAIChat, Confidence: 0.5}})
	r.Register("*:*:*", &stubNormalizer{id: "g", payload: core.NormalizedPayload{Kind: core.KindHTTPText, Protocol: "generic"}})
	r.Freeze()
	// With clamp to 1.0 and confidence 0.5, k should soft-fall to Tier 3 generic.
	got, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Either generic wins (Tier 3) OR bestPartial returned (the k payload).
	// With threshold=1.0 the k payload Confidence 0.5 will not pass, generic
	// always succeeds and effConf(1.0) >= bestConf(0.5).
	if got.Protocol != "generic" {
		t.Fatalf("expected Tier-3 fallback; got Protocol=%q", got.Protocol)
	}
}

func TestRegistry_SetConfidenceThreshold_PanicsOnFrozen(t *testing.T) {
	r := core.NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.SetConfidenceThreshold(0.5)
}

func TestRegistry_RegisterTier2_PanicsOnFrozen(t *testing.T) {
	r := core.NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.RegisterTier2(&stubNormalizer{})
}

func TestRegistry_RegisterTier2_FiresWhenTier1LowConfidence(t *testing.T) {
	r := core.NewRegistry()
	tier1 := &stubNormalizer{id: "t1", payload: core.NormalizedPayload{Kind: core.KindAIChat, Confidence: 0.4, Protocol: "t1"}}
	tier2 := &stubNormalizer{id: "t2", payload: core.NormalizedPayload{Kind: core.KindAIChat, Confidence: 0.9, Protocol: "t2"}}
	r.Register("k", tier1)
	r.RegisterTier2(tier2)
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "t2" {
		t.Fatalf("expected Tier-2 winner (Confidence 0.9 >= 0.7), got %q", got.Protocol)
	}
}

func TestRegistry_Tier2_LowConfidenceUsesBestPartial(t *testing.T) {
	r := core.NewRegistry()
	tier1 := &stubNormalizer{id: "t1", payload: core.NormalizedPayload{Kind: core.KindAIChat, Confidence: 0.4, Protocol: "t1"}}
	tier2 := &stubNormalizer{id: "t2", payload: core.NormalizedPayload{Kind: core.KindAIChat, Confidence: 0.5, Protocol: "t2"}}
	r.Register("k", tier1)
	r.RegisterTier2(tier2)
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Best partial wins (t2 0.5 > t1 0.4).
	if got.Protocol != "t2" {
		t.Fatalf("expected bestPartial t2, got %q", got.Protocol)
	}
}

func TestRegistry_Tier2_ErrUnsupportedFallsThrough(t *testing.T) {
	r := core.NewRegistry()
	r.RegisterTier2(&stubNormalizer{id: "t2", err: core.ErrUnsupported})
	r.Register("*:*:*", &stubNormalizer{id: "g", payload: core.NormalizedPayload{Kind: core.KindHTTPText, Protocol: "generic"}})
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "unknown"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "generic" {
		t.Fatalf("expected Tier-3 fallback, got %q", got.Protocol)
	}
}

func TestRegistry_Tier2_HardErrorTerminates(t *testing.T) {
	r := core.NewRegistry()
	hardErr := errors.New("bad bytes")
	r.RegisterTier2(&stubNormalizer{id: "t2", err: hardErr, payload: core.NormalizedPayload{Kind: core.KindAIChat, Protocol: "t2"}})
	r.Register("*:*:*", &stubNormalizer{id: "g", payload: core.NormalizedPayload{Kind: core.KindHTTPText, Protocol: "generic"}})
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "unknown"})
	if !errors.Is(err, hardErr) {
		t.Fatalf("hard error should propagate, got %v", err)
	}
}

func TestRegistry_Tier3_HardErrorTerminates(t *testing.T) {
	r := core.NewRegistry()
	hardErr := errors.New("generic blew up")
	r.Register("*:*:*", &stubNormalizer{id: "g", err: hardErr, payload: core.NormalizedPayload{Kind: core.KindHTTPText}})
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "x"})
	if !errors.Is(err, hardErr) {
		t.Fatalf("Tier-3 hard error must propagate; got %v", err)
	}
}

func TestRegistry_Normalize_Tier1HardErrorTerminates(t *testing.T) {
	r := core.NewRegistry()
	hard := errors.New("malformed")
	r.Register("k", &stubNormalizer{id: "k", err: hard, payload: core.NormalizedPayload{Kind: core.KindAIChat, Protocol: "k"}})
	r.Register("*:*:*", &stubNormalizer{id: "g", payload: core.NormalizedPayload{Kind: core.KindHTTPText, Protocol: "g"}})
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "k"})
	if !errors.Is(err, hard) {
		t.Fatalf("Tier-1 hard error should short-circuit; got %v", err)
	}
}

func TestRegistry_Normalize_BestPartialReturned(t *testing.T) {
	r := core.NewRegistry()
	// Tier-1 returns sub-threshold; no Tier-2; no Tier-3.
	r.Register("k", &stubNormalizer{id: "k", payload: core.NormalizedPayload{Kind: core.KindAIChat, Confidence: 0.5, Protocol: "k"}})
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "k" {
		t.Fatalf("expected bestPartial returned, got %q", got.Protocol)
	}
}

func TestRegistry_Normalize_DeduplicatesNormalizers(t *testing.T) {
	// Same normalizer under two keys → must run once.
	r := core.NewRegistry()
	called := 0
	stub := &countingNormalizer{
		stubNormalizer: stubNormalizer{id: "c", payload: core.NormalizedPayload{Kind: core.KindAIChat, Confidence: 0.9, Protocol: "c"}},
		called:         &called,
	}
	r.Register("openai", stub)
	r.Register("openai::/v1/chat/completions", stub)
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), core.Meta{AdapterType: "openai", EndpointPath: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called != 1 {
		t.Fatalf("normalizer called %d times; should be deduplicated to 1", called)
	}
}

// stubNormalizer is a minimal core.Normalizer for use in tests.
type stubNormalizer struct {
	id      string
	payload core.NormalizedPayload
	err     error
}

func (s *stubNormalizer) ID() string { return s.id }
func (s *stubNormalizer) Normalize(_ context.Context, _ []byte, _ core.Meta) (core.NormalizedPayload, error) {
	return s.payload, s.err
}

// countingNormalizer tracks invocations to assert Tier-1 dedup behaviour.
type countingNormalizer struct {
	stubNormalizer
	called *int
}

func (c *countingNormalizer) Normalize(ctx context.Context, raw []byte, m core.Meta) (core.NormalizedPayload, error) {
	*c.called++
	return c.stubNormalizer.Normalize(ctx, raw, m)
}

func TestMaybeGunzip_GzipDecompresses(t *testing.T) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	out, ok := core.MaybeGunzip(buf.Bytes())
	if !ok {
		t.Fatal("gzip not detected")
	}
	if string(out) != "hello world" {
		t.Fatalf("decompressed = %q", out)
	}
}

func TestMaybeGunzip_GzipTruncatedKeepsRaw(t *testing.T) {
	// gzip magic but truncated header → gzip.NewReader fails → keep raw.
	raw := []byte{0x1f, 0x8b, 0x00}
	out, ok := core.MaybeGunzip(raw)
	if ok {
		t.Fatal("truncated gzip should fail-open")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw should be returned unchanged")
	}
}

func TestMaybeGunzip_GzipReadErrorKeepsRaw(t *testing.T) {
	// gzip header valid but trailer corrupt — gzip.NewReader succeeds but
	// io.ReadAll fails. Hand-craft a body whose tail is invalid.
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write([]byte("hello"))
	_ = w.Close()
	bad := buf.Bytes()
	// Corrupt the CRC32 trailer (last 8 bytes).
	for i := len(bad) - 4; i < len(bad); i++ {
		bad[i] = 0xff
	}
	out, ok := core.MaybeGunzip(bad)
	if ok {
		t.Fatal("corrupt gzip trailer should fail-open")
	}
	if !bytes.Equal(out, bad) {
		t.Fatalf("raw must be unchanged on read err")
	}
}

func TestMaybeGunzip_ZlibDecompresses(t *testing.T) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte("zlib body")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	out, ok := core.MaybeGunzip(buf.Bytes())
	if !ok {
		t.Fatal("zlib not detected")
	}
	if string(out) != "zlib body" {
		t.Fatalf("zlib decoded = %q", out)
	}
}

func TestMaybeGunzip_ZlibTruncatedKeepsRaw(t *testing.T) {
	// zlib magic but truncated — zlib.NewReader fails.
	raw := []byte{0x78, 0x9c}
	out, ok := core.MaybeGunzip(raw)
	if ok {
		t.Fatal("truncated zlib should fail-open")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw not preserved")
	}
}

func TestMaybeGunzip_ZlibReadErrorKeepsRaw(t *testing.T) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, _ = w.Write([]byte("xyz"))
	_ = w.Close()
	bad := buf.Bytes()
	// Corrupt the deflate stream body — keep the zlib header (first 2 bytes).
	for i := 2; i < len(bad); i++ {
		bad[i] = 0xff
	}
	out, ok := core.MaybeGunzip(bad)
	if ok {
		t.Fatal("corrupt zlib body should fail-open")
	}
	if !bytes.Equal(out, bad) {
		t.Fatalf("raw must be unchanged on read err")
	}
}

func TestMaybeGunzip_ZstdDecompresses(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := enc.EncodeAll([]byte("zstd body"), nil)
	_ = enc.Close()
	out, ok := core.MaybeGunzip(compressed)
	if !ok {
		t.Fatal("zstd not detected")
	}
	if string(out) != "zstd body" {
		t.Fatalf("zstd decoded = %q", out)
	}
}

func TestMaybeGunzip_ZstdTruncatedKeepsRaw(t *testing.T) {
	// zstd magic but invalid stream.
	raw := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00}
	out, ok := core.MaybeGunzip(raw)
	if ok {
		t.Fatal("invalid zstd should fail-open")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw not preserved")
	}
}

func TestMaybeGunzip_TooShortKeepsRaw(t *testing.T) {
	out, ok := core.MaybeGunzip([]byte{0x1f}) // < 2 bytes
	if ok || !bytes.Equal(out, []byte{0x1f}) {
		t.Fatalf("short body should fail-open; got ok=%v out=%v", ok, out)
	}
}

func TestMaybeGunzip_UnknownMagicKeepsRaw(t *testing.T) {
	raw := []byte("plain text content")
	out, ok := core.MaybeGunzip(raw)
	if ok {
		t.Fatal("plain text must not be flagged as compressed")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw not preserved")
	}
}

func TestRegistry_Normalize_AutoDecompressGzip(t *testing.T) {
	// Pipe a gzipped body through Normalize and verify the underlying
	// normalizer sees decompressed bytes.
	r := core.NewRegistry()
	var got []byte
	r.Register("k", &captureNormalizer{
		stubNormalizer: stubNormalizer{id: "k", payload: core.NormalizedPayload{Kind: core.KindAIChat, Protocol: "k"}},
		seen:           &got,
	})
	r.Freeze()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write([]byte(`{"x":1}`))
	_ = w.Close()
	_, err := r.Normalize(context.Background(), buf.Bytes(), core.Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(got) != `{"x":1}` {
		t.Fatalf("normalizer saw %q want decompressed JSON", got)
	}
}

type captureNormalizer struct {
	stubNormalizer
	seen *[]byte
}

func (c *captureNormalizer) Normalize(ctx context.Context, raw []byte, m core.Meta) (core.NormalizedPayload, error) {
	*c.seen = append([]byte(nil), raw...)
	return c.stubNormalizer.Normalize(ctx, raw, m)
}

// apply_spans.go — form map mutation + core.ClonePayload variants + resolveTextRef edges

// applySpansMu serialises core.ApplySpans tests because pendingMapWrites is a
// package-level slice — running multiple core.ApplySpans goroutines concurrently
// would race on it. Production callers only ever invoke core.ApplySpans
// single-threaded; the lock here mirrors that contract.
var applySpansMu sync.Mutex

func TestApplySpans_HTTPFormMapEntry(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind: core.KindHTTPForm,
		HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{Form: map[string]string{"email": "user@example.com"}}},
	}
	spans := []core.TransformSpan{{
		Source: core.SourceHook, Action: core.ActionRedact,
		ContentAddress: "http.bodyView.form.email",
		Start:          0, End: 4, Replacement: "[REDACTED]",
	}}
	got, skipped := core.ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if got.HTTP.BodyView.Form["email"] != "[REDACTED]@example.com" {
		t.Fatalf("form entry not mutated: %q", got.HTTP.BodyView.Form["email"])
	}
	// Original map unchanged.
	if p.HTTP.BodyView.Form["email"] != "user@example.com" {
		t.Fatalf("original map mutated: %q", p.HTTP.BodyView.Form["email"])
	}
}

func TestApplySpans_HTTPFormMapEntryMissing(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind: core.KindHTTPForm,
		HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{Form: map[string]string{"k": "v"}}},
	}
	spans := []core.TransformSpan{{ContentAddress: "http.bodyView.form.missing", Start: 0, End: 1, Replacement: "x"}}
	got, skipped := core.ApplySpans(p, spans)
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped, got %+v", skipped)
	}
	if got.HTTP.BodyView.Form["k"] != "v" {
		t.Fatalf("unrelated entry shouldn't change")
	}
}

func TestApplySpans_HTTPFormNilFormSkipped(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind: core.KindHTTPForm,
		HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{Form: nil}},
	}
	spans := []core.TransformSpan{{ContentAddress: "http.bodyView.form.k", Start: 0, End: 1, Replacement: "x"}}
	_, skipped := core.ApplySpans(p, spans)
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped, got %+v", skipped)
	}
}

func TestApplySpans_ResolveTextRef_InvalidAddresses(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind:     core.KindAIChat,
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "x"}}}},
	}
	cases := []string{
		"messages",                 // too short
		"messages.0",               // missing content
		"messages.x.content.0",     // bad index parse
		"messages.0.content.x",     // bad j parse
		"messages.0.content.99",    // j out of range
		"messages.99.content.0",    // i out of range
		"messages.-1.content.0",    // negative i (core.ParseInt fails)
		"messages.0.wrong.0",       // not "content"
		"messages.0.content.0.foo", // unknown trailing field
		"http.bodyView",            // wrong kind (no HTTP)
		"http.bodyView.form.x",     // wrong kind
		"http.unknownPath",         // valid prefix but wrong second token
		"unknown.top",              // unknown top-level
	}
	for _, addr := range cases {
		span := []core.TransformSpan{{ContentAddress: addr, Start: 0, End: 1, Replacement: "z"}}
		_, skipped := core.ApplySpans(p, span)
		if len(skipped) != 1 {
			t.Errorf("addr=%q: expected 1 skipped, got %v", addr, skipped)
		}
	}
}

func TestApplySpans_ResolveTextRef_ToolResult(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind: core.KindAIChat,
		Messages: []core.Message{{
			Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentToolResult, ToolResult: &core.ToolResult{Output: "abc"}}},
		}},
	}
	spans := []core.TransformSpan{{ContentAddress: "messages.0.content.0.toolResult", Start: 0, End: 3, Replacement: "XYZ"}}
	got, skipped := core.ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if got.Messages[0].Content[0].ToolResult.Output != "XYZ" {
		t.Fatalf("tool result not mutated: %+v", got.Messages[0].Content[0].ToolResult)
	}
}

func TestApplySpans_ResolveTextRef_ToolResultNil(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind: core.KindAIChat,
		Messages: []core.Message{{
			Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentToolResult, ToolResult: nil}},
		}},
	}
	spans := []core.TransformSpan{{ContentAddress: "messages.0.content.0.toolResult", Start: 0, End: 1, Replacement: "x"}}
	_, skipped := core.ApplySpans(p, spans)
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped, got %+v", skipped)
	}
}

func TestApplySpans_StartLargerThanText(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind:     core.KindAIChat,
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "ab"}}}},
	}
	// start > len(text) is skipped per applyToAddress
	spans := []core.TransformSpan{{ContentAddress: "messages.0.content.0", Start: 5, End: 6, Replacement: "X"}}
	got, _ := core.ApplySpans(p, spans)
	// text unchanged
	if got.Messages[0].Content[0].Text != "ab" {
		t.Fatalf("text shouldn't change for out-of-range start: %q", got.Messages[0].Content[0].Text)
	}
}

func TestApplySpans_StartGreaterThanEndSkipped(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind:     core.KindAIChat,
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "abcdef"}}}},
	}
	// invalid span: start=4, end=2 — per applyToAddress comment skipped.
	spans := []core.TransformSpan{{ContentAddress: "messages.0.content.0", Start: 4, End: 2, Replacement: "X"}}
	got, _ := core.ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "abcdef" {
		t.Fatalf("text changed despite invalid span: %q", got.Messages[0].Content[0].Text)
	}
}

func TestApplySpans_NegativeStartClamped(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind:     core.KindAIChat,
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "abcdef"}}}},
	}
	// start = -2 clamps to 0; end=2 replaces "ab" with "X"
	spans := []core.TransformSpan{{ContentAddress: "messages.0.content.0", Start: -2, End: 2, Replacement: "X"}}
	got, _ := core.ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "Xcdef" {
		t.Fatalf("expected Xcdef, got %q", got.Messages[0].Content[0].Text)
	}
}

func TestApplySpans_EndPastLengthClamped(t *testing.T) {
	applySpansMu.Lock()
	defer applySpansMu.Unlock()
	p := core.NormalizedPayload{
		Kind:     core.KindAIChat,
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "abc"}}}},
	}
	spans := []core.TransformSpan{{ContentAddress: "messages.0.content.0", Start: 1, End: 999, Replacement: "Z"}}
	got, _ := core.ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "aZ" {
		t.Fatalf("expected aZ, got %q", got.Messages[0].Content[0].Text)
	}
}

func TestClonePayload_AllFields(t *testing.T) {
	// Build a payload with every cloneable slice/map populated, then mutate
	// the clone and verify originals stay untouched.
	p := core.NormalizedPayload{
		Kind: core.KindAIChat,
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.ContentBlock{
			{Type: core.ContentText, Text: "hi"},
			{Type: core.ContentToolResult, ToolResult: &core.ToolResult{Output: "tr"}},
		}}},
		Tools:   []core.ToolDef{{Name: "t1"}},
		RuleIDs: []string{"r1", "r2"},
		HTTP: &core.HTTPPayload{
			Method:          "POST",
			URL:             "https://x",
			HeadersFiltered: map[string]string{"h": "v"},
			BodyView: &core.HTTPBodyView{
				Text: "body",
				Form: map[string]string{"k": "v"},
			},
		},
	}
	clone := core.ClonePayload(p)
	// Mutate clone deeply.
	clone.Messages[0].Content[0].Text = "MUT"
	clone.Messages[0].Content[1].ToolResult.Output = "MUT"
	clone.Tools[0].Name = "MUT"
	clone.RuleIDs[0] = "MUT"
	clone.HTTP.HeadersFiltered["h"] = "MUT"
	clone.HTTP.BodyView.Text = "MUT"
	clone.HTTP.BodyView.Form["k"] = "MUT"

	if p.Messages[0].Content[0].Text != "hi" ||
		p.Messages[0].Content[1].ToolResult.Output != "tr" {
		t.Errorf("messages mutated through clone: %+v", p.Messages)
	}
	if p.Tools[0].Name != "t1" {
		t.Errorf("tools mutated: %+v", p.Tools)
	}
	if p.RuleIDs[0] != "r1" {
		t.Errorf("ruleIDs mutated: %+v", p.RuleIDs)
	}
	if p.HTTP.HeadersFiltered["h"] != "v" {
		t.Errorf("headers mutated: %+v", p.HTTP.HeadersFiltered)
	}
	if p.HTTP.BodyView.Text != "body" {
		t.Errorf("body view text mutated: %+v", p.HTTP.BodyView)
	}
	if p.HTTP.BodyView.Form["k"] != "v" {
		t.Errorf("form mutated: %+v", p.HTTP.BodyView.Form)
	}
}

func TestClonePayload_NilSlicesPreserved(t *testing.T) {
	p := core.NormalizedPayload{Kind: core.KindAIChat}
	c := core.ClonePayload(p)
	if c.Messages != nil || c.Tools != nil || c.RuleIDs != nil || c.HTTP != nil {
		t.Fatalf("nil slices should stay nil: %+v", c)
	}
}

func TestClonePayload_HTTPWithoutBodyView(t *testing.T) {
	p := core.NormalizedPayload{
		Kind: core.KindHTTPText,
		HTTP: &core.HTTPPayload{Method: "GET"}, // BodyView, Headers nil
	}
	c := core.ClonePayload(p)
	if c.HTTP == nil || c.HTTP.Method != http.MethodGet {
		t.Fatalf("HTTP not cloned: %+v", c.HTTP)
	}
	if c.HTTP.BodyView != nil || c.HTTP.HeadersFiltered != nil {
		t.Fatalf("nil sub-fields should stay nil: %+v", c.HTTP)
	}
}

func TestClonePayload_HTTPBodyViewWithoutForm(t *testing.T) {
	p := core.NormalizedPayload{
		Kind: core.KindHTTPText,
		HTTP: &core.HTTPPayload{BodyView: &core.HTTPBodyView{Text: "raw"}}, // no Form
	}
	c := core.ClonePayload(p)
	if c.HTTP.BodyView == nil || c.HTTP.BodyView.Text != "raw" {
		t.Fatalf("body view text lost: %+v", c.HTTP.BodyView)
	}
	if c.HTTP.BodyView.Form != nil {
		t.Fatalf("nil form should stay nil: %+v", c.HTTP.BodyView)
	}
}

func TestParseInt_EmptyAndNonDigit(t *testing.T) {
	if _, err := core.ParseInt(""); err == nil {
		t.Fatal("empty must error")
	}
	if _, err := core.ParseInt("12a"); err == nil {
		t.Fatal("non-digit must error")
	}
	v, err := core.ParseInt("42")
	if err != nil || v != 42 {
		t.Fatalf("core.ParseInt(42)=%d,%v", v, err)
	}
}

func TestKind_IsHTTP_AllArms(t *testing.T) {
	cases := map[core.Kind]bool{
		core.KindHTTPJSON:      true,
		core.KindHTTPText:      true,
		core.KindHTTPForm:      true,
		core.KindHTTPMultipart: true,
		core.KindHTTPBinary:    true,
		core.KindAIChat:        false,
		core.KindAIEmbedding:   false,
		core.KindUnsupported:   false,
	}
	for k, want := range cases {
		if got := k.IsHTTP(); got != want {
			t.Errorf("Kind(%q).IsHTTP() = %v want %v", k, got, want)
		}
	}
}

func TestKind_IsAI_AllArms(t *testing.T) {
	if !core.KindAIChat.IsAI() || !core.KindAICompletion.IsAI() || !core.KindAIImage.IsAI() {
		t.Fatal("all ai-* should report IsAI true")
	}
	if core.KindHTTPJSON.IsAI() || core.KindUnsupported.IsAI() {
		t.Fatal("http/unsupported should not report IsAI true")
	}
}

func TestMessage_MarshalJSON_NilContentBecomesEmptyArray(t *testing.T) {
	m := core.Message{Role: core.RoleAssistant, Content: nil}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content":[]`) {
		t.Fatalf("nil content should become []; got %s", b)
	}
}

func TestMessage_MarshalJSON_NonNilContentPreserved(t *testing.T) {
	m := core.Message{Role: core.RoleUser, Content: []core.ContentBlock{{Type: core.ContentText, Text: "hi"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"text":"hi"`) {
		t.Fatalf("content not preserved: %s", b)
	}
}

// anthropic_messages.go — MergeAnthropicEventUsage + content variants

func TestMergeAnthropicEventUsage_MessageStart(t *testing.T) {
	prev := MergeAnthropicEventUsage(nil, []byte(`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":2}}}`))
	if prev == nil {
		t.Fatal("expected non-nil usage")
		return
	}
	if prev.PromptTokens == nil || *prev.PromptTokens != 7 {
		t.Fatalf("PromptTokens = %+v want 7 (5 uncached + 2 cache_read)", prev.PromptTokens)
	}
	if prev.CacheReadTokens == nil || *prev.CacheReadTokens != 2 {
		t.Fatalf("CacheReadTokens = %+v want 2", prev.CacheReadTokens)
	}
}

func TestMergeAnthropicEventUsage_MessageDeltaIncremental(t *testing.T) {
	prev := MergeAnthropicEventUsage(nil, []byte(`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`))
	prev = MergeAnthropicEventUsage(prev, []byte(`{"type":"message_delta","usage":{"output_tokens":3}}`))
	if prev == nil || *prev.PromptTokens != 5 || *prev.CompletionTokens != 3 || *prev.TotalTokens != 8 {
		t.Fatalf("merged = %+v want 5/3/8", prev)
	}
}

func TestMergeAnthropicEventUsage_NoUsage(t *testing.T) {
	// Event with no usage field returns prev unchanged.
	five := 5
	prev := &core.Usage{PromptTokens: &five}
	got := MergeAnthropicEventUsage(prev, []byte(`{"type":"content_block_delta"}`))
	if got != prev {
		t.Fatalf("unchanged prev expected; got %p vs %p", got, prev)
	}
}

func TestMergeAnthropicEventUsage_BadJSON(t *testing.T) {
	five := 5
	prev := &core.Usage{PromptTokens: &five}
	got := MergeAnthropicEventUsage(prev, []byte(`{not-json`))
	if got != prev {
		t.Fatalf("malformed event should leave prev unchanged")
	}
}

func TestMergeAnthropicEventUsage_NullEnvelope(t *testing.T) {
	prev := &core.Usage{}
	got := MergeAnthropicEventUsage(prev, []byte(`null`))
	if got != prev {
		t.Fatalf("null envelope should leave prev unchanged")
	}
}

func TestMergeAnthropicUsage_CacheWriteUpdatesPrompt(t *testing.T) {
	// Start with a baseline prev, then push cache_creation_input_tokens.
	prev := &core.Usage{}
	prev = mergeAnthropicUsage(prev, map[string]any{"input_tokens": float64(10)})
	if *prev.PromptTokens != 10 {
		t.Fatalf("first step PromptTokens = %v", prev.PromptTokens)
	}
	prev = mergeAnthropicUsage(prev, map[string]any{"cache_creation_input_tokens": float64(4)})
	if *prev.PromptTokens != 14 || *prev.CacheCreationTokens != 4 {
		t.Fatalf("after cache_creation: PromptTokens=%v CacheCreation=%v want 14,4", prev.PromptTokens, prev.CacheCreationTokens)
	}
}

func TestAnthropicContentPart_VariantsExhaustive(t *testing.T) {
	cases := []struct {
		name string
		part map[string]any
		want core.ContentType
	}{
		{"text", map[string]any{"type": "text", "text": "hi"}, core.ContentText},
		{"thinking with thinking field", map[string]any{"type": "thinking", "thinking": "raw"}, core.ContentReasoning},
		{"thinking falling back to text field", map[string]any{"type": "thinking", "text": "fallback"}, core.ContentReasoning},
		{"image", map[string]any{"type": "image", "source": map[string]any{"media_type": "image/png", "data": "BASE64IMAGEDATA-LONGER-THAN-16-BYTES"}}, core.ContentImageRef},
		{"image with empty source", map[string]any{"type": "image"}, core.ContentImageRef},
		{"tool_use", map[string]any{"type": "tool_use", "id": "u1", "name": "f", "input": map[string]any{"k": "v"}}, core.ContentToolUse},
		{"tool_result string content", map[string]any{"type": "tool_result", "tool_use_id": "u1", "content": "ok"}, core.ContentToolResult},
		{"tool_result array content", map[string]any{"type": "tool_result", "tool_use_id": "u2", "content": []any{map[string]any{"text": "a"}, map[string]any{"text": "b"}}}, core.ContentToolResult},
		{"unknown type preserves JSON", map[string]any{"type": "weird", "extra": 1}, core.ContentText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := anthropicContentPart(c.part)
			if got.Type != c.want {
				t.Fatalf("Type = %v want %v", got.Type, c.want)
			}
			// Spot-check specific fields where appropriate.
			switch c.name {
			case "tool_result array content":
				if got.ToolResult == nil || got.ToolResult.Output != "ab" {
					t.Fatalf("array content collapse: %+v", got.ToolResult)
				}
			case "image":
				if got.ImageRef == nil || got.ImageRef.ContentType != "image/png" || got.ImageRef.SHA256 == "" {
					t.Fatalf("image ref: %+v", got.ImageRef)
				}
			case "image with empty source":
				if got.ImageRef == nil || got.ImageRef.ContentType != "image" {
					t.Fatalf("default content type lost: %+v", got.ImageRef)
				}
			case "tool_use":
				if got.ToolUse == nil || got.ToolUse.Name != "f" || got.ToolUse.Input["k"] != "v" {
					t.Fatalf("tool use: %+v", got.ToolUse)
				}
			case "unknown type preserves JSON":
				if !strings.Contains(got.Text, "weird") {
					t.Fatalf("unknown should serialise JSON; got %q", got.Text)
				}
			}
		})
	}
}

func TestAnthropicSystemToBlocks_EmptyString(t *testing.T) {
	if got := anthropicSystemToBlocks([]byte(`""`)); got != nil {
		t.Fatalf("empty string system should yield nil; got %+v", got)
	}
}

func TestAnthropicDecodeContent_NullAndEmpty(t *testing.T) {
	if got := anthropicDecodeContent(nil); got != nil {
		t.Fatalf("nil should yield nil; got %+v", got)
	}
	if got := anthropicDecodeContent([]byte("null")); got != nil {
		t.Fatalf("null should yield nil; got %+v", got)
	}
	if got := anthropicDecodeContent([]byte(`""`)); got != nil {
		t.Fatalf("empty string should yield nil; got %+v", got)
	}
}

func TestAnthropicDecodeContent_UnparseableFallsThrough(t *testing.T) {
	got := anthropicDecodeContent([]byte(`{"not":"array"}`))
	if len(got) != 1 || got[0].Type != core.ContentText {
		t.Fatalf("unparseable should preserve raw as text fallback: %+v", got)
	}
}

func TestAnthropic_UnknownDirectionYieldsErrUnsupported(t *testing.T) {
	_, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: core.Direction("xx")})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestAnthropic_NonStreamResponseMalformed(t *testing.T) {
	_, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte("garbage"), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected unmarshal err")
	}
}

func TestAnthropic_StreamResponseMalformedEventStillYieldsPartial(t *testing.T) {
	// Inject a non-JSON data line — the decodable frames still fold and
	// the malformed one weighs on the coverage confidence instead of
	// failing the parse.
	raw := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"model":"c","usage":{"input_tokens":3}}}`,
		"",
		"event: content_block_delta",
		"data: <not json>",
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"",
	}, "\n")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("decodable frames must fold without error, got %v", err)
	}
	if len(got.Messages) == 0 || got.Messages[0].Content[0].Text != "ok" {
		t.Fatalf("partial parse should still extract usable text; got %+v", got.Messages)
	}
	if want := 3.0 / 4.0; got.Confidence != want {
		t.Errorf("confidence = %v, want %v (3 of 4 frames recognized)", got.Confidence, want)
	}
}

// openai_chat.go — usage alias chain + indexOfToolCall + role + content variants

func TestOpenAIUsage_ExtractCanonicalUsage_AllAliases(t *testing.T) {
	// Every alias chain in isolation.
	cases := []struct {
		name string
		u    openAIUsage
		want core.Usage
	}{
		{
			name: "openai canonical",
			u: openAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, PromptTokensDetails: &struct {
				CachedTokens        int `json:"cached_tokens,omitempty"`
				CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
			}{CachedTokens: 3}},
			want: core.Usage{PromptTokens: ptrInt(10), CompletionTokens: ptrInt(5), TotalTokens: ptrInt(15), CacheReadTokens: ptrInt(3)},
		},
		{
			name: "responses-shape top-level fallback",
			u:    openAIUsage{InputTokens: 8, OutputTokens: 4, TotalTokens: 12},
			want: core.Usage{PromptTokens: ptrInt(8), CompletionTokens: ptrInt(4), TotalTokens: ptrInt(12)},
		},
		{
			name: "deepseek prompt_cache_hit_tokens",
			u:    openAIUsage{PromptTokens: 100, CompletionTokens: 10, PromptCacheHitTokens: 40},
			want: core.Usage{PromptTokens: ptrInt(100), CompletionTokens: ptrInt(10), CacheReadTokens: ptrInt(40)},
		},
		{
			name: "moonshot prompt_cache_tokens",
			u:    openAIUsage{PromptTokens: 100, PromptCacheTokens: 50},
			want: core.Usage{PromptTokens: ptrInt(100), CacheReadTokens: ptrInt(50)},
		},
		{
			name: "kimi flat cached_tokens",
			u:    openAIUsage{PromptTokens: 100, FlatCachedTokens: 60},
			want: core.Usage{PromptTokens: ptrInt(100), CacheReadTokens: ptrInt(60)},
		},
		{
			name: "responses input_tokens_details cached_tokens",
			u: openAIUsage{InputTokens: 80, InputTokensDetails: &struct {
				CachedTokens int `json:"cached_tokens,omitempty"`
			}{CachedTokens: 30}},
			want: core.Usage{PromptTokens: ptrInt(80), CacheReadTokens: ptrInt(30)},
		},
		{
			name: "cache_creation_tokens (Anthropic via openai shim)",
			u: openAIUsage{PromptTokens: 50, PromptTokensDetails: &struct {
				CachedTokens        int `json:"cached_tokens,omitempty"`
				CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
			}{CacheCreationTokens: 12}},
			want: core.Usage{PromptTokens: ptrInt(50), CacheCreationTokens: ptrInt(12)},
		},
		{
			name: "openai o-series reasoning",
			u: openAIUsage{PromptTokens: 10, CompletionTokens: 100, CompletionTokensDetails: &struct {
				ReasoningTokens int `json:"reasoning_tokens,omitempty"`
			}{ReasoningTokens: 75}},
			want: core.Usage{PromptTokens: ptrInt(10), CompletionTokens: ptrInt(100), ReasoningTokens: ptrInt(75)},
		},
		{
			name: "responses reasoning",
			u: openAIUsage{InputTokens: 10, OutputTokens: 100, OutputTokensDetails: &struct {
				ReasoningTokens int `json:"reasoning_tokens,omitempty"`
			}{ReasoningTokens: 60}},
			want: core.Usage{PromptTokens: ptrInt(10), CompletionTokens: ptrInt(100), ReasoningTokens: ptrInt(60)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.u.extractCanonicalUsage()
			if got == nil {
				t.Fatal("nil")
				return
			}
			if !equalIntPtr(got.PromptTokens, c.want.PromptTokens) ||
				!equalIntPtr(got.CompletionTokens, c.want.CompletionTokens) ||
				!equalIntPtr(got.TotalTokens, c.want.TotalTokens) ||
				!equalIntPtr(got.CacheReadTokens, c.want.CacheReadTokens) ||
				!equalIntPtr(got.CacheCreationTokens, c.want.CacheCreationTokens) ||
				!equalIntPtr(got.ReasoningTokens, c.want.ReasoningTokens) {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestOpenAIUsage_AllZerosReturnsNil(t *testing.T) {
	if got := (openAIUsage{}).extractCanonicalUsage(); got != nil {
		t.Fatalf("all-zero usage should yield nil, got %+v", got)
	}
}

func TestIndexOfToolCall_NoIndexFieldFallsBackToPos(t *testing.T) {
	// When the delta tool_call has no Index field (nil), indexOfToolCall must
	// fall back to the caller-supplied pos argument.  This is the non-compliant
	// / synthetic-stream fallback path; the happy path (explicit Index) is
	// covered by the MultiToolCall integration tests.
	for _, tt := range []struct {
		tc   openAIToolCall
		pos  int
		want int
	}{
		{openAIToolCall{ID: "x"}, 0, 0},
		{openAIToolCall{ID: "x"}, 3, 3},
	} {
		if got := indexOfToolCall(tt.tc, tt.pos); got != tt.want {
			t.Fatalf("indexOfToolCall(%v, pos=%d) = %d, want %d", tt.tc, tt.pos, got, tt.want)
		}
	}
}

func TestIndexOfToolCall_ExplicitIndexWinsOverPos(t *testing.T) {
	// When the delta tool_call carries an explicit Index, that value must win
	// over pos regardless of the caller's position in the chunk slice.
	idx := 7
	tc := openAIToolCall{ID: "x", Index: &idx}
	if got := indexOfToolCall(tc, 0); got != 7 {
		t.Fatalf("indexOfToolCall with explicit Index=7, pos=0 = %d, want 7", got)
	}
}

func TestRoleFromString_UnknownRoleReturnsRaw(t *testing.T) {
	if got := roleFromString("custom-bot"); got != core.Role("custom-bot") {
		t.Fatalf("unknown role should pass through; got %q", got)
	}
}

func TestRoleFromString_FunctionMapsToTool(t *testing.T) {
	if got := roleFromString("function"); got != core.RoleTool {
		t.Fatalf("function should map to tool; got %q", got)
	}
}

func TestOpenAIContentPart_ImageURLBareString(t *testing.T) {
	// Top-level url missing → ImageRef created with empty SpillKey.
	got := openAIContentPart(map[string]any{"type": "image_url"})
	if got.Type != core.ContentImageRef || got.ImageRef == nil || got.ImageRef.ContentType != "image" {
		t.Fatalf("image_url bare: %+v", got)
	}
}

func TestOpenAIContentPart_UnknownTypePreservesJSON(t *testing.T) {
	got := openAIContentPart(map[string]any{"type": "video", "x": 1})
	if got.Type != core.ContentText {
		t.Fatalf("unknown type should fallback to text: %+v", got)
	}
	if !strings.Contains(got.Text, "video") {
		t.Fatalf("unknown should serialise JSON; got %q", got.Text)
	}
}

func TestOpenAIChat_DecodeContent_StringWithToolCallID(t *testing.T) {
	// role=tool with string content → ToolResult block.
	blocks := decodeOpenAIContent(json.RawMessage(`"weather is sunny"`), nil, "call_abc", "")
	if len(blocks) != 1 || blocks[0].Type != core.ContentToolResult || blocks[0].ToolResult == nil || blocks[0].ToolResult.Output != "weather is sunny" || blocks[0].ToolResult.CallID != "call_abc" {
		t.Fatalf("tool result decode wrong: %+v", blocks)
	}
}

func TestOpenAIChat_DecodeContent_ReasoningPrefix(t *testing.T) {
	blocks := decodeOpenAIContent(json.RawMessage(`"visible"`), nil, "", "thinking trace")
	if len(blocks) != 2 || blocks[0].Type != core.ContentReasoning || blocks[0].Text != "thinking trace" || blocks[1].Type != core.ContentText {
		t.Fatalf("reasoning prefix lost: %+v", blocks)
	}
}

func TestOpenAIChat_DecodeContent_ToolCallNonFunctionSkipped(t *testing.T) {
	tc := openAIToolCall{Type: "not-function"}
	blocks := decodeOpenAIContent(json.RawMessage(`""`), []openAIToolCall{tc}, "", "")
	if len(blocks) != 0 {
		t.Fatalf("non-function tool call should be skipped, got %+v", blocks)
	}
}

func TestOpenAIChat_DecodeContent_NullContent(t *testing.T) {
	// content "null" with reasoning and tool call should still produce blocks.
	blocks := decodeOpenAIContent(json.RawMessage(`null`), nil, "", "reasoning-only")
	if len(blocks) != 1 || blocks[0].Type != core.ContentReasoning {
		t.Fatalf("null content with reasoning should yield reasoning-only: %+v", blocks)
	}
}

func TestOpenAIChat_DecodeContent_EmptyStringContent(t *testing.T) {
	// Empty string content with tool_call_id should NOT generate a tool_result block.
	blocks := decodeOpenAIContent(json.RawMessage(`""`), nil, "call_x", "")
	if len(blocks) != 0 {
		t.Fatalf("empty string content should produce no blocks; got %+v", blocks)
	}
}

func TestOpenAIChat_NonStreamResponse_ChoicesPresentButMessageNil(t *testing.T) {
	// Choices array has elements but each has Message=nil. The normalizer
	// should not crash and should still emit zero Messages.
	body := `{"model":"x","choices":[{"index":0,"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("nil messages → 0 entries, got %+v", got.Messages)
	}
	if got.FinishReason != "stop" {
		t.Fatalf("FinishReason should still be set from choices[0]: %q", got.FinishReason)
	}
	if got.Usage == nil || *got.Usage.PromptTokens != 5 {
		t.Fatalf("usage extraction broke: %+v", got.Usage)
	}
}

func TestOpenAIChat_StreamResponse_ReasoningStream(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"deepseek-r1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think a"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":" think b"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages: %+v", got.Messages)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 2 || blocks[0].Type != core.ContentReasoning || blocks[0].Text != "think a think b" || blocks[1].Type != core.ContentText || blocks[1].Text != "answer" {
		t.Fatalf("reasoning + text stream wrong: %+v", blocks)
	}
}

func TestOpenAIChat_StreamResponse_ToolCallStitch(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
	}, "\n")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 || got.Messages[0].Content[0].Type != core.ContentToolUse {
		t.Fatalf("expected stitched tool_use block: %+v", got.Messages)
	}
	tu := got.Messages[0].Content[0].ToolUse
	if tu.Name != "f" || tu.CallID != "call_1" || tu.Input["a"] != float64(1) {
		t.Fatalf("tool_use stitch wrong: %+v", tu)
	}
}

func TestOpenAIChat_StreamResponse_BadJSONLineFoldsPrefix(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		``,
		`data: <not json>`,
		``,
	}, "\n")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("decodable prefix must fold without error, got %v", err)
	}
	if len(got.Messages) == 0 || got.Messages[0].Content[0].Text != "a" {
		t.Fatalf("graceful fold should preserve usable text; got %+v", got.Messages)
	}
	if got.Confidence != 0.5 {
		t.Fatalf("confidence = %v, want 0.5 (bad line counts toward total only)", got.Confidence)
	}
}

// gemini_generate.go — ExtractGeminiEventUsage + role + response side

func TestExtractGeminiEventUsage_PresentAndAbsent(t *testing.T) {
	with := []byte(`{"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10,"thoughtsTokenCount":2}}`)
	got := ExtractGeminiEventUsage(with)
	if got == nil || *got.PromptTokens != 7 || *got.CompletionTokens != 5 || *got.ReasoningTokens != 2 || *got.TotalTokens != 10 {
		t.Fatalf("usage extract wrong: %+v", got)
	}
	if ExtractGeminiEventUsage(nil) != nil {
		t.Fatal("nil chunk should return nil usage")
	}
	if ExtractGeminiEventUsage([]byte(`{"x":1}`)) != nil {
		t.Fatal("chunk without usageMetadata should return nil")
	}
	if ExtractGeminiEventUsage([]byte(`<bad>`)) != nil {
		t.Fatal("bad JSON should return nil")
	}
}

func TestGeminiRoleToCanonical_AllArms(t *testing.T) {
	cases := map[string]core.Role{
		"model":        core.RoleAssistant,
		"user":         core.RoleUser,
		"":             core.RoleUser,
		"function":     core.RoleTool,
		"system":       core.RoleSystem,
		"unknown-role": core.Role("unknown-role"),
		"USER":         core.RoleUser,
	}
	for in, want := range cases {
		if got := geminiRoleToCanonical(in); got != want {
			t.Errorf("geminiRoleToCanonical(%q) = %q want %q", in, got, want)
		}
	}
}

func TestGemini_ResponseInlineDataAndFunctionCall(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c","name":"f","args":{"a":1}}},{"functionResponse":{"id":"c","name":"f","response":{"r":1}}},{"inlineData":{"mimeType":"image/png","data":"BASE64DATAVERY-LONG"}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 3 {
		t.Fatalf("expected 3 content blocks (tool_use, tool_result, image); got %+v", got.Messages)
	}
	types := []core.ContentType{got.Messages[0].Content[0].Type, got.Messages[0].Content[1].Type, got.Messages[0].Content[2].Type}
	want := []core.ContentType{core.ContentToolUse, core.ContentToolResult, core.ContentImageRef}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("block[%d] = %q want %q (full: %+v)", i, types[i], want[i], got.Messages[0].Content)
		}
	}
}

func TestGemini_ResponseCandidateWithNilContent(t *testing.T) {
	body := `{"candidates":[{"finishReason":"SAFETY","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":0,"totalTokenCount":1}}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("nil content should skip the candidate; got %+v", got.Messages)
	}
	if got.FinishReason != "SAFETY" {
		t.Fatalf("FinishReason should still be set: %q", got.FinishReason)
	}
}

func TestGemini_UnknownDirection(t *testing.T) {
	_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: core.Direction("nope")})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestGemini_RequestMalformed(t *testing.T) {
	_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte("xx"), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected unmarshal err")
	}
}

func TestGemini_ResponseMalformed(t *testing.T) {
	_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte("xx"), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected unmarshal err")
	}
}

func TestGemini_StreamWithFunctionCallAndResponse(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},"index":0}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"functionResponse":{"name":"f","response":{"r":1}}}]},"index":0}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AAA"}}]},"finishReason":"STOP","index":0}]}`,
		``,
	}, "\n")
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("expected 1 message: %+v", got.Messages)
	}
	// Order: text → tool_use → tool_result → image
	types := []core.ContentType{}
	for _, b := range got.Messages[0].Content {
		types = append(types, b.Type)
	}
	if len(types) < 4 ||
		types[0] != core.ContentText ||
		types[1] != core.ContentToolUse ||
		types[2] != core.ContentToolResult ||
		types[3] != core.ContentImageRef {
		t.Fatalf("stream block stitch order wrong: %+v", types)
	}
}

func TestGemini_StreamMalformedDataLine(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"ok"}]},"index":0}]}`,
		``,
		`data: <bad json>`,
		``,
	}, "\n")
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("decodable prefix must fold without error, got %v", err)
	}
	if got.Messages[0].Content[0].Text != "ok" {
		t.Fatalf("graceful fold should preserve text: %+v", got.Messages)
	}
	if got.Confidence != 0.5 {
		t.Fatalf("confidence = %v, want 0.5 (bad line counts toward total only)", got.Confidence)
	}
}

// openai_responses.go — ID + tool_call alternative + edge cases

func TestOpenAIResponses_ID(t *testing.T) {
	if id := NewOpenAIResponsesNormalizer().ID(); id != "openai-responses" {
		t.Fatalf("ID = %q", id)
	}
}

func TestOpenAIResponses_EmptyBody(t *testing.T) {
	for _, d := range []core.Direction{core.DirectionRequest, core.DirectionResponse} {
		_, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), nil, core.Meta{Direction: d})
		if !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("dir %v: expected core.ErrUnsupported, got %v", d, err)
		}
	}
}

func TestOpenAIResponses_UnknownDirection(t *testing.T) {
	_, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: core.Direction("???")})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestOpenAIResponses_RequestMalformed(t *testing.T) {
	_, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), []byte("xx"), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected unmarshal err")
	}
}

func TestOpenAIResponses_ResponseMalformed(t *testing.T) {
	_, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), []byte("xx"), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected unmarshal err")
	}
}

func TestOpenAIResponses_InputContentBlocksVariants(t *testing.T) {
	parts := []openaiResponsesInputContent{
		{Type: "input_text", Text: "hi"},
		{Type: "", Text: "default-text"},
		{Type: "input_image"},
		{Type: "unknown", Text: "fallback"}, // unknown with text preserved
		{Type: "input_text"},                // empty text skipped
		{Type: "unknown"},                   // unknown without text skipped
	}
	got := openaiResponsesInputContentToBlocks(parts)
	if len(got) != 4 {
		t.Fatalf("expected 4 blocks, got %d: %+v", len(got), got)
	}
	if got[0].Type != core.ContentText || got[1].Type != core.ContentText ||
		got[2].Type != core.ContentImageRef || got[3].Type != core.ContentText {
		t.Fatalf("block types wrong: %+v", got)
	}
}

func TestOpenAIResponses_InputContentEmptySlice(t *testing.T) {
	if got := openaiResponsesInputContentToBlocks(nil); got != nil {
		t.Fatalf("nil parts → nil blocks; got %+v", got)
	}
}

func TestOpenAIResponses_RequestEmptyContentSkipped(t *testing.T) {
	// Input item with empty content[] should be skipped, not produce zero-content message.
	body := []byte(`{"model":"x","input":[{"role":"user","content":[]},{"role":"user","content":[{"type":"input_text","text":"keep"}]}]}`)
	got, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "keep" {
		t.Fatalf("empty content item should be skipped; got %+v", got.Messages)
	}
}

func TestOpenAIResponses_RequestUnnamedTools(t *testing.T) {
	// Tools without a name are skipped.
	body := []byte(`{"model":"x","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function"},{"name":"valid","type":"function"}]}`)
	got, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "valid" {
		t.Fatalf("unnamed tool should be skipped; got %+v", got.Tools)
	}
}

func TestOpenAIResponses_ResponseToolCallArgumentsFallback(t *testing.T) {
	// Item has Arguments string but no Input map → unmarshal arguments.
	body := []byte(`{"id":"r","status":"completed","model":"x","output":[{"type":"function_call","name":"f","id":"fc","arguments":"{\"k\":\"v\"}"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	got, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), body, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	tu := got.Messages[0].Content[0].ToolUse
	if tu == nil || tu.Input["k"] != "v" {
		t.Fatalf("arguments fallback unmarshal failed: %+v", tu)
	}
	// CallID falls back to item.ID when call_id absent.
	if tu.CallID != "fc" {
		t.Fatalf("CallID fallback wrong: %q want fc", tu.CallID)
	}
}

func TestOpenAIResponses_ResponseUnknownItemTypesIgnored(t *testing.T) {
	// Unknown output item types should be skipped without crashing.
	body := []byte(`{"id":"r","status":"completed","model":"x","output":[{"type":"unknown_thing"},{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	got, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), body, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Messages[0].Content[0].Text != "ok" {
		t.Fatalf("unknown types lost the valid message: %+v", got.Messages)
	}
}

// generic_http.go — split params + looksLikeText + form/multipart edges

func TestSplitMediaTypeAndParams_Cases(t *testing.T) {
	cases := []struct {
		in       string
		wantMt   string
		hasParam bool
	}{
		{"", "", false},
		{"application/json", "application/json", false},
		{"application/json; charset=utf-8", "application/json", true},
		{"multipart/form-data; boundary=abc", "multipart/form-data", true},
		// Trailing whitespace — mime.ParseMediaType rejects, fall through to manual split.
		{"application/json   ", "application/json", false},
		// Wholly garbage
		{"~~~", "~~~", false},
	}
	for _, c := range cases {
		mt, params := splitMediaTypeAndParams(c.in)
		if mt != c.wantMt {
			t.Errorf("in=%q: media=%q want %q", c.in, mt, c.wantMt)
		}
		if c.hasParam && params == nil {
			t.Errorf("in=%q: expected non-nil params", c.in)
		}
	}
}

func TestLooksLikeText_All(t *testing.T) {
	if !looksLikeText("text/csv") || !looksLikeText("text/plain") {
		t.Fatal("text/* should be true")
	}
	if !looksLikeText("application/json") || !looksLikeText("application/vnd.api+json") {
		t.Fatal("json variants should be true")
	}
	if !looksLikeText("application/x-www-form-urlencoded") {
		t.Fatal("form should be true")
	}
	if looksLikeText("image/png") || looksLikeText("application/octet-stream") {
		t.Fatal("binary types should be false")
	}
}

func TestLooksLikeUTF8Text_RejectsControlBytes(t *testing.T) {
	if looksLikeUTF8Text([]byte{'a', 'b', 0x01, 'c'}) {
		t.Fatal("control byte should disqualify")
	}
	if !looksLikeUTF8Text([]byte("hello\twith\nws")) {
		t.Fatal("plain text with whitespace should pass")
	}
	// Long body: only first 512 bytes inspected — pad past 512 with junk.
	probe := append([]byte("hello"), make([]byte, 600)...)
	if looksLikeUTF8Text(probe) {
		t.Fatal("zero-byte padding after first 512 stays in probe window")
	}
}

func TestNormalizeForm_MalformedReturnsErrAndText(t *testing.T) {
	// url.ParseQuery accepts almost anything as form; force an error by
	// passing a body containing an invalid percent-escape.
	got, err := NewGenericHTTPNormalizer().Normalize(context.Background(), []byte("%xx=bad"), core.Meta{ContentType: "application/x-www-form-urlencoded"})
	if err == nil {
		t.Fatal("expected form-decode err")
	}
	// On error the BodyView.Text fallback should carry the raw body.
	if got.HTTP == nil || got.HTTP.BodyView == nil || got.HTTP.BodyView.Text == "" {
		t.Fatalf("expected Text fallback: %+v", got.HTTP)
	}
}

func TestNormalizeMultipart_PartReadError(t *testing.T) {
	// Truncated multipart body — first part header valid, then EOF mid-content.
	// multipart.NewReader returns err from NextPart on truncation; we should
	// receive the err and the partial form.
	body := "--bnd\r\nContent-Disposition: form-data; name=\"f\"\r\n\r\nvalue without trailer"
	got, err := NewGenericHTTPNormalizer().Normalize(context.Background(), []byte(body), core.Meta{ContentType: "multipart/form-data; boundary=bnd"})
	if err == nil {
		t.Fatal("expected partial-parse err on truncated multipart")
	}
	if got.Kind != core.KindHTTPMultipart {
		t.Fatalf("Kind: %v", got.Kind)
	}
}

func ptrInt(v int) *int { return &v }

func equalIntPtr(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
