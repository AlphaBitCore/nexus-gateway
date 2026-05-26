package cohere

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "cohere" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestExtractRequest_StringContent(t *testing.T) {
	body := []byte(`{
		"model":"command-r-plus",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Hello!"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[1] != "Hello!" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "command-r-plus" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_ArrayContent(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_ToolCallsInHistory(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"assistant","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
			]},
			{"role":"tool","tool_results":[
				{"document":{"data":"72F sunny"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	found := false
	for _, s := range nc.Segments {
		if s == "72F sunny" {
			found = true
		}
	}
	if !found {
		t.Errorf("tool result text missing from Segments: %v", nc.Segments)
	}
}

func TestExtractRequest_ToolDefinitionsInMetadata(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","description":""}}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(nc.Metadata["tools"], `"f"`) {
		t.Errorf("Metadata[tools]=%q", nc.Metadata["tools"])
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/v2/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"command-r"}`), "/v2/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractResponse_TextAndToolPlanAndToolCalls(t *testing.T) {
	body := []byte(`{
		"id":"resp_1",
		"message":{
			"role":"assistant",
			"content":[{"type":"text","text":"The weather is …"}],
			"tool_plan":"I will call get_weather to check NYC.",
			"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}
			]
		},
		"finish_reason":"COMPLETE"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "The weather is …" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ReasoningSegments) != 1 || !strings.Contains(nc.ReasoningSegments[0], "get_weather") {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	if nc.Metadata["finish_reason"] != "COMPLETE" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
}

func TestExtractStreamChunk_ContentDelta(t *testing.T) {
	chunk := []byte(`{"type":"content-delta","delta":{"message":{"content":{"text":"Hello"}}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_ToolPlanDelta(t *testing.T) {
	chunk := []byte(`{"type":"tool-plan-delta","delta":{"message":{"tool_plan":"thinking …"}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "thinking …" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

func TestExtractStreamChunk_ToolCallStart(t *testing.T) {
	chunk := []byte(`{"type":"tool-call-start","delta":{"message":{"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"get_weather","arguments":""}}
	]}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_MessageEndFinishReason(t *testing.T) {
	chunk := []byte(`{"type":"message-end","delta":{"finish_reason":"COMPLETE"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["finish_reason"] != "COMPLETE" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
}

func TestExtractStreamChunk_MessageStartSkipped(t *testing.T) {
	chunk := []byte(`{"type":"message-start","id":"resp_1"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
		t.Errorf("non-content frame leaked: %+v", nc)
	}
}

func TestDetectRequestMeta_BearerKey(t *testing.T) {
	body := []byte(`{"model":"command-r-plus","messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.cohere.com/v2/chat", nil)
	r.Header.Set("Authorization", "Bearer co_xxx")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "cohere" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "cohere-bearer" {
		t.Errorf("ApiKeyClass=%q", meta.ApiKeyClass)
	}
}

func TestDetectResponseUsage(t *testing.T) {
	body := []byte(`{
		"usage":{"tokens":{"input_tokens":42,"output_tokens":13}}
	}`)
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Errorf("Status=%q", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 42 {
		t.Errorf("PromptTokens=%v", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 13 {
		t.Errorf("CompletionTokens=%v", um.CompletionTokens)
	}
}

func TestDetectResponseUsage_ParseFailed(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`{}`)).Status != traffic.UsageStatusParseFailed {
		t.Errorf("want parse_failed")
	}
}

// Configure is a no-op for cohere — exercise both nil and a populated
// config map to pin the contract that future config additions must
// preserve the error-free no-op behavior.
func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// RewriteRequestBody / RewriteResponseBody are intentionally
// unsupported for cohere (no rewriting on Cohere chat traffic).
// Lock the contract: returns the body unchanged, zero patches, and the
// sentinel ErrRewriteUnsupported the dispatcher checks via errors.Is.
func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v2/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("patches=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body mutated: %q want %q", out, body)
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"message":{"content":[{"type":"text","text":"hi"}]}}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v2/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("patches=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body mutated: %q want %q", out, body)
	}
}

// ExtractResponse_Malformed: invalid JSON returns ErrMalformed so the
// pipeline can flag the body as non-parseable instead of silently
// returning empty content.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`), "/v2/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractResponse_MissingMessage: valid JSON but missing the required
// `message` envelope is an unknown schema — the adapter must surface
// ErrUnknownSchema so the dispatcher can demote to a generic spec
// instead of silently emitting zero segments as a parsed response.
func TestExtractResponse_MissingMessage(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"id":"resp_1"}`), "/v2/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractResponse_IDMetadataOnly: when the message exists but has no
// text/tool_plan/tool_calls payload, the id should still flow into
// Metadata so downstream dedup/cache can key on it.
func TestExtractResponse_IDMetadataOnly(t *testing.T) {
	body := []byte(`{"id":"resp_2","message":{"role":"assistant","content":[]}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["id"] != "resp_2" {
		t.Errorf("Metadata[id]=%q want resp_2", nc.Metadata["id"])
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// ExtractStreamChunk_Malformed: invalid JSON in a single SSE event
// frame surfaces ErrMalformed instead of being silently swallowed.
func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/v2/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractStreamChunk_ToolCallDelta: argument fragments arrive on
// tool-call-delta events. The adapter must append each call frame raw
// so the downstream accumulator can stitch arguments across chunks.
func TestExtractStreamChunk_ToolCallDelta(t *testing.T) {
	chunk := []byte(`{"type":"tool-call-delta","delta":{"message":{"tool_calls":[
		{"index":0,"function":{"arguments":"{\"city\":"}}
	]}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"arguments"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// ExtractStreamChunk_UnknownType: a frame whose `type` we don't model
// (content-start, citations, …) must return a zero NormalizedContent
// with a nil Metadata map — non-nil-but-empty maps trip downstream
// equality assertions.
func TestExtractStreamChunk_UnknownType(t *testing.T) {
	chunk := []byte(`{"type":"citations","citations":[{"start":0}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 || len(nc.ReasoningSegments) != 0 {
		t.Errorf("unknown frame leaked content: %+v", nc)
	}
	if nc.Metadata != nil {
		t.Errorf("Metadata=%v want nil for unknown frame", nc.Metadata)
	}
}

// ExtractStreamChunk_ContentDeltaEmptyText: a content-delta with an
// empty `text` field must not emit a zero-length Segment — the empty
// string is meaningless to downstream consumers and would pad token
// counters.
func TestExtractStreamChunk_ContentDeltaEmptyText(t *testing.T) {
	chunk := []byte(`{"type":"content-delta","delta":{"message":{"content":{"text":""}}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty for empty text", nc.Segments)
	}
}

// ExtractStreamChunk_ToolPlanDeltaEmpty: tool-plan-delta with empty
// text — same anti-noise rule as content-delta empty.
func TestExtractStreamChunk_ToolPlanDeltaEmpty(t *testing.T) {
	chunk := []byte(`{"type":"tool-plan-delta","delta":{"message":{"tool_plan":""}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ReasoningSegments) != 0 {
		t.Errorf("ReasoningSegments=%v want empty", nc.ReasoningSegments)
	}
}

// ExtractStreamChunk_MessageEndEmpty: message-end without a
// finish_reason field must NOT emit a Metadata map with an empty
// finish_reason — downstream queries on `finish_reason IS NULL` would
// regress to false positives.
func TestExtractStreamChunk_MessageEndEmpty(t *testing.T) {
	chunk := []byte(`{"type":"message-end","delta":{}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata != nil {
		t.Errorf("Metadata=%v want nil when finish_reason absent", nc.Metadata)
	}
}

// ExtractRequest_ToolResultTextField: tool_results can carry a plain
// `text` field instead of `document.data`. Both must be extracted to
// Segments so the prompt-cache key includes tool outputs regardless of
// which serialization the caller chose.
func TestExtractRequest_ToolResultTextField(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"tool","tool_results":[
				{"text":"72F sunny"}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, s := range nc.Segments {
		if s == "72F sunny" {
			found = true
		}
	}
	if !found {
		t.Errorf("tool result text missing from Segments: %v", nc.Segments)
	}
}

// ExtractRequest_DocumentsMetadata: top-level `documents` array (RAG
// docs) must be carried in Metadata raw so dedup keys include the
// retrieved context — two requests differing only in docs are different
// prompts.
func TestExtractRequest_DocumentsMetadata(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"summarize"}],
		"documents":[{"id":"d1","data":"some doc"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(nc.Metadata["documents"], `"d1"`) {
		t.Errorf("Metadata[documents]=%q", nc.Metadata["documents"])
	}
}

// DetectRequestMeta_NoAuth: missing Authorization header must not stamp
// ApiKeyClass / Fingerprint — leaving stale "cohere-bearer" on an
// unauthenticated request would poison downstream attribution.
func TestDetectRequestMeta_NoAuth(t *testing.T) {
	body := []byte(`{"model":"command-r","messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.cohere.com/v2/chat", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint != "" {
		t.Errorf("ApiKeyFingerprint=%q want empty", meta.ApiKeyFingerprint)
	}
	if meta.Model != "command-r" {
		t.Errorf("Model=%q", meta.Model)
	}
}

// DetectRequestMeta_NilRequest: defensive — body-only call (no http
// request) must still surface Provider + Model.
func TestDetectRequestMeta_NilRequest(t *testing.T) {
	body := []byte(`{"model":"command-r-plus"}`)
	a := &Adapter{}
	meta := a.DetectRequestMeta(nil, body)
	if meta.Provider != "cohere" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "command-r-plus" {
		t.Errorf("Model=%q", meta.Model)
	}
}

// DetectRequestMeta_BearerEmptyToken: "Bearer " with no token after
// must NOT stamp the api-key fields — a blank fingerprint would
// collide across every unauthenticated request.
func TestDetectRequestMeta_BearerEmptyToken(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.cohere.com/v2/chat", nil)
	r.Header.Set("Authorization", "Bearer ")
	meta := a.DetectRequestMeta(r, nil)
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty for blank token", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint != "" {
		t.Errorf("ApiKeyFingerprint=%q want empty for blank token", meta.ApiKeyFingerprint)
	}
}

// DetectRequestMeta_NonBearerAuth: a non-Bearer Authorization scheme
// (Basic, etc.) is not a cohere API key and must not be classified.
func TestDetectRequestMeta_NonBearerAuth(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.cohere.com/v2/chat", nil)
	r.Header.Set("Authorization", "Basic xyz")
	meta := a.DetectRequestMeta(r, nil)
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty for non-Bearer scheme", meta.ApiKeyClass)
	}
}

// DetectRequestMeta_MalformedBody: a non-JSON body must NOT poison the
// Model field — the http header parsing path is independent of the
// body parser.
func TestDetectRequestMeta_MalformedBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.cohere.com/v2/chat", nil)
	r.Header.Set("Authorization", "Bearer co_x")
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Model != "" {
		t.Errorf("Model=%q want empty for malformed body", meta.Model)
	}
	if meta.ApiKeyClass != "cohere-bearer" {
		t.Errorf("ApiKeyClass=%q want cohere-bearer (independent of body)", meta.ApiKeyClass)
	}
}

// DetectResponseUsage_NoBody: zero-length body returns the dedicated
// NoBody status (distinct from ParseFailed) so observability can tell
// "we never saw a body" from "we saw garbage".
func TestDetectResponseUsage_NoBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

// DetectResponseUsage_MalformedJSON: a non-empty but non-JSON body
// returns ParseFailed.
func TestDetectResponseUsage_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`not json`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

// DetectResponseUsage_PromptTokensOnly: when only input_tokens is
// present, the status stays OK and CompletionTokens stays nil — proves
// the OK gate fires on either side, not requiring both.
func TestDetectResponseUsage_PromptTokensOnly(t *testing.T) {
	body := []byte(`{"usage":{"tokens":{"input_tokens":17}}}`)
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Errorf("Status=%q want ok", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 17 {
		t.Errorf("PromptTokens=%v want 17", um.PromptTokens)
	}
	if um.CompletionTokens != nil {
		t.Errorf("CompletionTokens=%v want nil", um.CompletionTokens)
	}
}

// DetectResponseUsage_CompletionTokensOnly: symmetric to the prompt
// path — only output_tokens present.
func TestDetectResponseUsage_CompletionTokensOnly(t *testing.T) {
	body := []byte(`{"usage":{"tokens":{"output_tokens":9}}}`)
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Errorf("Status=%q want ok", um.Status)
	}
	if um.PromptTokens != nil {
		t.Errorf("PromptTokens=%v want nil", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 9 {
		t.Errorf("CompletionTokens=%v want 9", um.CompletionTokens)
	}
}

// Normalize: cohere advertises the openai-chat wire spec, so a body
// shaped like an OpenAI chat completion must claim Tier-1 with
// DetectedSpec=cohere and surface the user prompt + model.
func TestNormalize_OpenAIChatShape(t *testing.T) {
	body := []byte(`{
		"model":"command-r-plus",
		"messages":[{"role":"user","content":"hello cohere"}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v2/chat",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != adapterID {
		t.Errorf("DetectedSpec=%q want %q", payload.DetectedSpec, adapterID)
	}
	if payload.Model != "command-r-plus" {
		t.Errorf("Model=%q", payload.Model)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(payload.Messages))
	}
	if payload.Messages[0].Role != normalize.RoleUser {
		t.Errorf("role=%v want user", payload.Messages[0].Role)
	}
}

// Normalize_NonChatBody: a body that doesn't match the openai-chat
// shape (no `messages` array, no signature fields) must fall through
// with an error so the coordinator advances to Tier 2 / Tier 3.
func TestNormalize_NonChatBody(t *testing.T) {
	body := []byte(`{"foo":"bar","count":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected error for non-chat body")
	}
}
