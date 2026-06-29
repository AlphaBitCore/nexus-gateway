package codecs

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Shared codec singletons: per-host adapters delegate to the SAME
// instances the registry serves for the wire-format keys, so the
// registry's `tried` dedupe never runs one codec twice across the
// keyed walk and the sniff pass.

func TestSharedCodecInstancesAreTheRegistryInstances(t *testing.T) {
	reg := core.NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	if got := reg.Resolve(core.Meta{AdapterType: "openai"}); got != core.Normalizer(SharedOpenAIChat()) {
		t.Fatalf("registry openai instance %p != SharedOpenAIChat %p — dedupe contract broken", got, SharedOpenAIChat())
	}
	if got := reg.Resolve(core.Meta{AdapterType: "anthropic"}); got != core.Normalizer(SharedAnthropicMessages()) {
		t.Fatalf("registry anthropic instance != SharedAnthropicMessages — dedupe contract broken")
	}
	if got := reg.Resolve(core.Meta{AdapterType: "gemini"}); got != core.Normalizer(SharedGeminiGenerate()) {
		t.Fatalf("registry gemini instance != SharedGeminiGenerate — dedupe contract broken")
	}
}

// Anthropic Messages

func TestAnthropicRequest_MalformedJSONSurfacesNamedError(t *testing.T) {
	_, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(`{"model": "claude", "messages": [`),
		core.Meta{Direction: core.DirectionRequest})
	if err == nil || !strings.Contains(err.Error(), "request unmarshal") {
		t.Fatalf("err = %v, want the named request-unmarshal failure", err)
	}
}

func TestAnthropicNonStreamResponse_CacheWriteAndReasoningEstimate(t *testing.T) {
	body := `{
		"id": "msg_1", "type": "message", "role": "assistant",
		"model": "claude-sonnet-4-6",
		"content": [
			{"type": "thinking", "thinking": "Let me think about this for a moment."},
			{"type": "text", "text": "Answer."}
		],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 20,
		          "cache_creation_input_tokens": 5, "cache_read_input_tokens": 3}
	}`
	p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	u := p.Usage
	if u == nil {
		t.Fatal("usage missing")
	}
	if u.CacheCreationTokens == nil || *u.CacheCreationTokens != 5 {
		t.Fatalf("cacheCreationTokens = %v, want 5", u.CacheCreationTokens)
	}
	if u.PromptTokens == nil || *u.PromptTokens != 18 {
		t.Fatalf("promptTokens = %v, want 18 (uncached 10 + read 3 + write 5)", u.PromptTokens)
	}
	if u.TotalTokens == nil || *u.TotalTokens != 38 {
		t.Fatalf("totalTokens = %v, want 38", u.TotalTokens)
	}
	// 38 thinking chars × 2/7 ≈ 10 — the non-stream reasoning estimate
	// keeps the reasoning_ratio widget non-zero for Claude rows.
	if u.ReasoningTokens == nil || *u.ReasoningTokens != 10 {
		t.Fatalf("reasoningTokens = %v, want 10", u.ReasoningTokens)
	}
}

func TestAnthropicNonStreamResponse_TinyThinkingClampsEstimateToOne(t *testing.T) {
	body := `{
		"type": "message", "role": "assistant", "model": "claude-sonnet-4-6",
		"content": [{"type": "thinking", "thinking": "ab"}, {"type": "text", "text": "x"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 1, "output_tokens": 2}
	}`
	p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Usage == nil || p.Usage.ReasoningTokens == nil || *p.Usage.ReasoningTokens != 1 {
		t.Fatalf("reasoningTokens = %v, want clamp to 1 (2 chars × 2/7 rounds to 0)", p.Usage)
	}
}

func TestMergeAnthropicEventUsage_InconsistentPrevClampsUncached(t *testing.T) {
	// A caller-supplied running Usage where PromptTokens < cache parts
	// (possible when the caller seeded it from a foreign source) must
	// not produce a negative uncached count.
	one, five := 1, 5
	prev := &core.Usage{PromptTokens: &one, CacheReadTokens: &five}
	got := MergeAnthropicEventUsage(prev, []byte(`{"type":"message_delta","usage":{"output_tokens":7}}`))
	if got.PromptTokens == nil || *got.PromptTokens != 5 {
		t.Fatalf("promptTokens = %v, want 5 (uncached clamped to 0 + cacheRead 5)", got.PromptTokens)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 12 {
		t.Fatalf("totalTokens = %v, want 12", got.TotalTokens)
	}
}

func TestMergeAnthropicEventUsage_NoOpDeltaLeavesUsageUntouched(t *testing.T) {
	ten, two := 10, 2
	prev := &core.Usage{PromptTokens: &ten, CompletionTokens: &two}
	got := MergeAnthropicEventUsage(prev, []byte(`{"type":"message_delta","usage":{"output_tokens":0}}`))
	if got != prev {
		t.Fatal("a usage delta carrying only zero fields must return prev unchanged")
	}
	if *got.PromptTokens != 10 || *got.CompletionTokens != 2 {
		t.Fatalf("usage mutated by a no-op delta: %+v", got)
	}
}

// Gemini generateContent

func TestGeminiRequest_MissingContentsIsUnsupported(t *testing.T) {
	_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(`{"generationConfig":{"temperature":0.1}}`),
		core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported (request without contents[] is not a Gemini request)", err)
	}
}

func TestGeminiNonStreamResponse_EmptyRoleDefaultsToAssistant(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
	          "usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("messages = %+v, want one assistant message (empty response role = assistant)", p.Messages)
	}
}

// Gemini stream fold

func TestGeminiStream_DoneSentinelAndContentlessFinishFrame(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"index":0}]}`,
		`data: {"candidates":[{"finishReason":"STOP","index":0}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(p.Messages) != 1 || p.Messages[0].FinishReason != "STOP" {
		t.Fatalf("messages = %+v, want one message finishing STOP", p.Messages)
	}
	if len(p.Messages[0].Content) != 1 || p.Messages[0].Content[0].Text != "Hello" {
		t.Fatalf("content = %+v, want the single Hello text block", p.Messages[0].Content)
	}
}

func TestGeminiStream_SingleJSONFallbackDecodesNonStreamBody(t *testing.T) {
	// Vertex / AI Studio sometimes answer :streamGenerateContent with a
	// single JSON object. The stream decoder must fall through to the
	// non-stream shape: model from modelVersion, empty role = assistant,
	// nil-content candidates skipped, usage mapped.
	body := `{"modelVersion":"gemini-2.0-flash",
	          "candidates":[
	            {"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"},
	            {"finishReason":"SAFETY"}],
	          "usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}`
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Stream {
		t.Fatal("fallback single-JSON body must be stamped Stream=false")
	}
	if p.Model != "gemini-2.0-flash" {
		t.Fatalf("model = %q, want modelVersion", p.Model)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("messages = %+v, want one assistant message (nil-content candidate skipped)", p.Messages)
	}
	if p.Usage == nil || p.Usage.TotalTokens == nil || *p.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v, want totalTokens 8", p.Usage)
	}
}

func TestGeminiStream_OversizedUndecodableStreamIsUnsupported(t *testing.T) {
	// A data line beyond the 8 MiB scanner limit aborts the SSE walk;
	// the bytes are not a single JSON object either, so the row must
	// fall through (ErrUnsupported), not hang or half-claim.
	raw := "data: " + strings.Repeat("x", 8*1024*1024+16)
	_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// OpenAI Chat reasoning estimates

func TestOpenAIChatNonStream_ReasoningContentEstimatedWhenUsageOmitsIt(t *testing.T) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","model":"kimi-k2",
	          "choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"ab"},"finish_reason":"stop"}],
	          "usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Usage == nil || p.Usage.ReasoningTokens == nil || *p.Usage.ReasoningTokens != 1 {
		t.Fatalf("reasoningTokens = %+v, want clamp to 1 (2 chars × 2/7 rounds to 0)", p.Usage)
	}
}

func TestOpenAIChatNonStream_ReasoningEstimateCreatesUsageWhenBodyHasNone(t *testing.T) {
	// A usage-less body (some OpenAI-compatible vendors omit it) with
	// reasoning_content still surfaces a Usage carrying the estimate.
	body := `{"id":"chatcmpl-1","object":"chat.completion","model":"kimi-k2",
	          "choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"a long enough reasoning trace here"},"finish_reason":"stop"}]}`
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Usage == nil || p.Usage.ReasoningTokens == nil || *p.Usage.ReasoningTokens != 34*2/7 {
		t.Fatalf("usage = %+v, want a synthesized Usage with the reasoning estimate", p.Usage)
	}
}

func TestOpenAIChatStream_ReasoningDeltaEstimatedWhenUsageOmitsIt(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"ab"}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(),
		[]byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Usage == nil || p.Usage.ReasoningTokens == nil || *p.Usage.ReasoningTokens != 1 {
		t.Fatalf("reasoningTokens = %+v, want clamp to 1 (2 reasoning chars)", p.Usage)
	}
}

// OpenAI Responses input decode

func TestOpenAIResponsesRequest_StringShorthandBecomesUserMessage(t *testing.T) {
	p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
		[]byte(`{"model":"gpt-4o","input":"what is a gateway?"}`),
		core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleUser ||
		p.Messages[0].Content[0].Text != "what is a gateway?" {
		t.Fatalf("messages = %+v, want single user message from the string shorthand", p.Messages)
	}
}

func TestOpenAIResponsesRequest_RolelessItemDefaultsToUser(t *testing.T) {
	p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
		[]byte(`{"model":"gpt-4o","input":[{"content":[{"type":"input_text","text":"hi"}]}]}`),
		core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleUser {
		t.Fatalf("messages = %+v, want roleless item defaulted to user", p.Messages)
	}
}

func TestOpenAIResponsesRequest_UnusableInputShapesYieldNoMessages(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"null input", `{"model":"gpt-4o","instructions":"be brief","input":null}`},
		{"numeric input", `{"model":"gpt-4o","instructions":"be brief","input":42}`},
		{"array of non-items", `{"model":"gpt-4o","instructions":"be brief","input":[5]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
				[]byte(tc.body), core.Meta{Direction: core.DirectionRequest})
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			// Only the instructions-derived system message survives.
			if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleSystem {
				t.Fatalf("messages = %+v, want only the system message (input unusable)", p.Messages)
			}
		})
	}
}

func TestDecodeOpenAIResponsesInput_CorruptStringFragmentRefused(t *testing.T) {
	// The helper accepts raw bytes; a corrupt string fragment must be
	// refused (ok=false), not panic or fabricate a message.
	if items, ok := decodeOpenAIResponsesInput(json.RawMessage(`"unterminated`)); ok || items != nil {
		t.Fatalf("items=%v ok=%v, want nil/false on corrupt string input", items, ok)
	}
}

// Embedding normalizers: the polymorphic `input` field refuses
// unrecognised shapes with a named error so the registry records the
// row as failed instead of silently emitting an empty embedding row.

func TestEmbeddingInputDecode_UnrecognisedShapeNamedErrors(t *testing.T) {
	badInput := []byte(`{"model":"m","input":{"object":"not-an-input"}}`)
	meta := core.Meta{Direction: core.DirectionRequest}

	if _, err := NewOpenAIEmbeddingsNormalizer().Normalize(context.Background(), badInput, meta); err == nil ||
		!strings.Contains(err.Error(), "openai-embeddings: input decode") {
		t.Fatalf("openai: err = %v, want named input-decode failure", err)
	}
	if _, err := NewGLMEmbeddingsNormalizer().Normalize(context.Background(), badInput, meta); err == nil ||
		!strings.Contains(err.Error(), "glm-embeddings: input decode") {
		t.Fatalf("glm: err = %v, want named input-decode failure", err)
	}
	if _, err := NewVoyageEmbeddingsNormalizer().Normalize(context.Background(), badInput, meta); err == nil ||
		!strings.Contains(err.Error(), "voyage-embeddings: input decode") {
		t.Fatalf("voyage: err = %v, want named input-decode failure", err)
	}
}

func TestVoyageEmbeddings_StreamBytesAreUnsupported(t *testing.T) {
	_, err := NewVoyageEmbeddingsNormalizer().Normalize(context.Background(),
		[]byte(`{"model":"voyage-3","input":"x"}`),
		core.Meta{Direction: core.DirectionRequest, Stream: true})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported (Voyage has no streaming surface)", err)
	}
}

func TestGeminiEmbeddings_TypeMismatchBodiesNamedErrors(t *testing.T) {
	n := NewGeminiEmbeddingsNormalizer()
	ctx := context.Background()
	batchPath := "/v1beta/models/text-embedding-004:batchEmbedContents"

	if _, err := n.Normalize(ctx, []byte(`{"embedding": 5}`),
		core.Meta{Direction: core.DirectionResponse}); err == nil ||
		!strings.Contains(err.Error(), "single response unmarshal") {
		t.Fatalf("single response: err = %v, want named unmarshal failure", err)
	}
	if _, err := n.Normalize(ctx, []byte(`{"requests": 5}`),
		core.Meta{Direction: core.DirectionRequest, EndpointPath: batchPath}); err == nil ||
		!strings.Contains(err.Error(), "batch request unmarshal") {
		t.Fatalf("batch request: err = %v, want named unmarshal failure", err)
	}
	if _, err := n.Normalize(ctx, []byte(`{"embeddings": 5}`),
		core.Meta{Direction: core.DirectionResponse, EndpointPath: batchPath}); err == nil ||
		!strings.Contains(err.Error(), "batch response unmarshal") {
		t.Fatalf("batch response: err = %v, want named unmarshal failure", err)
	}
}

// Generic HTTP

func TestSplitMediaTypeAndParams_UnparseableValuePassesThrough(t *testing.T) {
	mt, params := splitMediaTypeAndParams("  bogus media type  ")
	if mt != "bogus media type" || params != nil {
		t.Fatalf("mt=%q params=%v, want the trimmed raw value with no params", mt, params)
	}
}

func TestGenericHTTP_NDJSONWithBlankLinesProjectsArray(t *testing.T) {
	raw := []byte("{\"a\":1}\n\n{\"b\":2}\n")
	p, err := NewGenericHTTPNormalizer().Normalize(context.Background(), raw,
		core.Meta{ContentType: "application/json", Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Kind != core.KindHTTPJSON {
		t.Fatalf("kind = %q, want http-json", p.Kind)
	}
	arr, ok := p.HTTP.BodyView.JSON.([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("bodyView = %+v, want a 2-element NDJSON array (blank line skipped)", p.HTTP.BodyView.JSON)
	}
}

func TestGenericHTTP_NDJSONOversizedLineFallsBackToText(t *testing.T) {
	// looksLikeNDJSON approves on the first two small lines; the third
	// line exceeds the 8 MiB scanner limit, so the decoder degrades to
	// the text projection instead of dropping the body.
	raw := []byte("{\"a\":1}\n{\"b\":2}\n{\"c\":\"" + strings.Repeat("x", 8*1024*1024+16) + "\"}")
	p, err := NewGenericHTTPNormalizer().Normalize(context.Background(), raw,
		core.Meta{ContentType: "application/json", Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Kind != core.KindHTTPText {
		t.Fatalf("kind = %q, want http-text fallback on scanner overflow", p.Kind)
	}
}

func TestGenericHTTP_MultipartPartNameFallbacks(t *testing.T) {
	boundary := "BOUNDARY42"
	body := strings.Join([]string{
		"--" + boundary,
		`Content-Disposition: attachment; filename="upload.txt"`,
		"",
		"file-bytes",
		"--" + boundary,
		"Content-Disposition: attachment",
		"",
		"anonymous-bytes",
		"--" + boundary + "--",
		"",
	}, "\r\n")
	p, err := NewGenericHTTPNormalizer().Normalize(context.Background(), []byte(body),
		core.Meta{ContentType: "multipart/form-data; boundary=" + boundary, Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Kind != core.KindHTTPMultipart {
		t.Fatalf("kind = %q, want http-multipart", p.Kind)
	}
	form := p.HTTP.BodyView.Form
	if form["upload.txt"] == "" {
		t.Fatalf("form = %v, want nameless part keyed by its filename", form)
	}
	if form["_part"] == "" {
		t.Fatalf("form = %v, want fully anonymous part keyed _part", form)
	}
}

// OpenAI projection

func TestProjectAssistantBlocks_ToolUseWithoutPayloadSkipped(t *testing.T) {
	text, _, toolCalls := projectAssistantBlocks([]core.ContentBlock{
		{Type: core.ContentToolUse}, // malformed: no ToolUse payload
		{Type: core.ContentText, Text: "still here"},
	})
	if len(toolCalls) != 0 {
		t.Fatalf("toolCalls = %v, want malformed block skipped", toolCalls)
	}
	if text != "still here" {
		t.Fatalf("text = %q, want surviving text block", text)
	}
}
