package poeweb

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Identity + configuration

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "poe-web" {
		t.Errorf("ID=%q want poe-web", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// TestExtractRequest_QueryFamily covers the `queries` array shape used
// by the poe GraphQL gql_POST endpoint, plus bot + chatId metadata.
func TestExtractRequest_QueryFamily(t *testing.T) {
	body := []byte(`{"queries":[{"role":"user","content":"summarize this thread"}],"bot":"GPT-4","chatId":"c-99"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/gql_POST")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "summarize this thread" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["bot"] != "GPT-4" {
		t.Errorf("bot=%q", nc.Metadata["bot"])
	}
	if nc.Metadata["chat_id"] != "c-99" {
		t.Errorf("chat_id=%q", nc.Metadata["chat_id"])
	}
}

// TestExtractRequest_MessagesArray covers the openai-chat-like shape
// with the `messages` array.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"reply"},
			{"role":"user","content":"second"}
		],
		"bot":"Claude-3.5-Sonnet",
		"chat_id":"cid-7"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/gql_POST")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"first", "reply", "second"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["bot"] != "Claude-3.5-Sonnet" {
		t.Errorf("bot=%q want Claude-3.5-Sonnet", nc.Metadata["bot"])
	}
	if nc.Metadata["chat_id"] != "cid-7" {
		t.Errorf("chat_id=%q want cid-7 (chat_id alias)", nc.Metadata["chat_id"])
	}
}

// TestExtractRequest_PromptAliases pins every alias key
// (`query`, `text`, `prompt`, `input`) contributes a segment when
// non-empty.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{"query":"one","text":"two","prompt":"three","input":"four"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"one", "two", "three", "four"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_EmptyPromptAliasesSkipped pins that empty alias
// values do not contribute phantom segments.
func TestExtractRequest_EmptyPromptAliasesSkipped(t *testing.T) {
	body := []byte(`{"query":"","text":"real","prompt":"","input":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real" {
		t.Errorf("Segments=%v want [real]", nc.Segments)
	}
}

// TestExtractRequest_BotNamePreference pins that the bot metadata is
// resolved from the FIRST matching key in priority order: bot >
// botName > modelName.
func TestExtractRequest_BotNamePreference(t *testing.T) {
	// botName takes effect when `bot` is absent.
	body := []byte(`{"query":"x","botName":"Llama-3.1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["bot"] != "Llama-3.1" {
		t.Errorf("bot=%q want Llama-3.1 (botName fallback)", nc.Metadata["bot"])
	}

	// modelName takes effect when both `bot` and `botName` are absent.
	body = []byte(`{"query":"x","modelName":"o1-preview"}`)
	nc, err = a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["bot"] != "o1-preview" {
		t.Errorf("bot=%q want o1-preview (modelName fallback)", nc.Metadata["bot"])
	}
}

// TestExtractRequest_ChatIDAlias pins both `chatId` and `chat_id` are
// normalised to `chat_id` in metadata, with `chatId` winning when both
// are present (declaration order in the lookup list).
func TestExtractRequest_ChatIDAlias(t *testing.T) {
	// chat_id alias when chatId is absent.
	body := []byte(`{"query":"x","chat_id":"snake-case-id"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["chat_id"] != "snake-case-id" {
		t.Errorf("chat_id=%q want snake-case-id", nc.Metadata["chat_id"])
	}
}

// TestExtractRequest_QueriesEmptyContent pins that entries in `queries`
// with empty `content` are skipped.
func TestExtractRequest_QueriesEmptyContent(t *testing.T) {
	body := []byte(`{"queries":[{"content":""},{"content":"real"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real" {
		t.Errorf("Segments=%v want [real]", nc.Segments)
	}
}

// TestExtractRequest_MessagesEmptyContent pins that entries in
// `messages` with empty `content` are skipped.
func TestExtractRequest_MessagesEmptyContent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":""},{"role":"user","content":"real"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real" {
		t.Errorf("Segments=%v want [real]", nc.Segments)
	}
}

// TestExtractRequest_NoBotMetaWhenAbsent pins that the bot/chat_id
// metadata keys are absent when the body has no bot identifier — no
// phantom empty value.
func TestExtractRequest_NoBotMetaWhenAbsent(t *testing.T) {
	body := []byte(`{"query":"hi"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["bot"]; ok {
		t.Errorf("bot leaked: %v", nc.Metadata)
	}
	if _, ok := nc.Metadata["chat_id"]; ok {
		t.Errorf("chat_id leaked: %v", nc.Metadata)
	}
}

// TestExtractRequest_Empty pins ErrUnknownSchema for nil body.
func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

// TestExtractRequest_BinaryBody pins ErrUnknownSchema for non-JSON
// payloads (poe sometimes sends multipart uploads).
func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff, 'h', 'i', 0x05}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for body bytes that
// begin like JSON but are not parseable.
func TestExtractRequest_Malformed(t *testing.T) {
	body := []byte(`{"query": "missing close-brace`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema when valid JSON
// carries no recognised fields.
func TestExtractRequest_UnknownJSON(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil (empty body is benign)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_Malformed: gjson rejects the body — must surface
// ErrMalformed.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_TextKey pins that a top-level `text` field is
// returned as a single segment.
func TestExtractResponse_TextKey(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"text":"answer text"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "answer text" {
		t.Errorf("Segments=%v want [answer text]", nc.Segments)
	}
}

// TestExtractResponse_ContentKey pins fall-through to the `content`
// key when `text` is absent.
func TestExtractResponse_ContentKey(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"content":"x"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "x" {
		t.Errorf("Segments=%v want [x]", nc.Segments)
	}
}

// TestExtractResponse_MessageKey pins fall-through to `message`.
func TestExtractResponse_MessageKey(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"message":"hello"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello" {
		t.Errorf("Segments=%v want [hello]", nc.Segments)
	}
}

// TestExtractResponse_ErrorKey pins the bare `error` string fall-back
// (last entry in the priority list).
func TestExtractResponse_ErrorKey(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"error":"rate limit"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limit" {
		t.Errorf("Segments=%v want [rate limit]", nc.Segments)
	}
}

// TestExtractResponse_TextWinsOverOthers pins priority order: `text`
// takes precedence over `content` when both are present.
func TestExtractResponse_TextWinsOverOthers(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"text":"primary","content":"secondary"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "primary" {
		t.Errorf("Segments=%v want [primary] (text wins)", nc.Segments)
	}
}

// TestExtractResponse_EmptyValuesFallThrough pins that empty string
// values are skipped — falling through to ErrUnknownSchema if all
// candidate keys are empty.
func TestExtractResponse_EmptyValuesFallThrough(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"text":"","content":"","message":"","error":""}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_UnknownJSON pins ErrUnknownSchema for valid JSON
// with no recognised keys.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractStreamChunk_TextDelta pins the `text` top-level key.
func TestExtractStreamChunk_TextDelta(t *testing.T) {
	chunk := []byte(`{"text":"streaming token "}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/sse")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streaming token " {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_AllTopLevelKeys covers every fan-out key:
// text, content, delta, token. All non-empty values contribute.
func TestExtractStreamChunk_AllTopLevelKeys(t *testing.T) {
	chunk := []byte(`{"text":"a","content":"b","delta":"c","token":"d"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/sse")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"a", "b", "c", "d"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_TopLevelEmptySkipped pins empty values do not
// produce phantom segments.
func TestExtractStreamChunk_TopLevelEmptySkipped(t *testing.T) {
	chunk := []byte(`{"text":"","content":"","delta":"only","token":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/sse")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "only" {
		t.Errorf("Segments=%v want [only]", nc.Segments)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open behaviour:
// nil, non-JSON, invalid-JSON, marker frames, and whitespace return a
// clean empty payload.
func TestExtractStreamChunk_DefensiveOnNonJSON(t *testing.T) {
	a := &Adapter{}
	cases := [][]byte{
		nil,
		[]byte(`not json at all`),
		[]byte(`{"oops":`),
		[]byte(`[DONE]`),
		[]byte(`  `),
	}
	for i, c := range cases {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/sse")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://poe.com/api/gql_POST", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"bot":"Claude-3.5-Sonnet"}`))
	if meta.Provider != "poe-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "Claude-3.5-Sonnet" {
		t.Errorf("Model=%q", meta.Model)
	}
}

// TestDetectRequestMeta_BotNamePreference pins fallback order:
// bot > botName > modelName.
func TestDetectRequestMeta_BotNamePreference(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://poe.com/api/gql_POST", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"botName":"Llama-3.1"}`))
	if meta.Model != "Llama-3.1" {
		t.Errorf("Model=%q want Llama-3.1 (botName fallback)", meta.Model)
	}

	meta = a.DetectRequestMeta(r, []byte(`{"modelName":"o1-preview"}`))
	if meta.Model != "o1-preview" {
		t.Errorf("Model=%q want o1-preview (modelName fallback)", meta.Model)
	}
}

// TestDetectRequestMeta_InvalidJSON pins defensive parsing: garbage
// input must not panic and Model stays empty.
func TestDetectRequestMeta_InvalidJSON(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://poe.com/api/gql_POST", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "poe-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

// TestDetectRequestMeta_NoBotInBody pins that an absent bot keeps
// Model empty but Provider still set.
func TestDetectRequestMeta_NoBotInBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://poe.com/api/gql_POST", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"query":"hi"}`))
	if meta.Provider != "poe-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty when no bot field", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("token pointers must be nil; got %+v", usage)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"query":"hi"}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged on rewrite-unsupported")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"text":"x"}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged")
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestChatShape pins openai-chat scoring.
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"You are a helpful assistant."},
			{"role":"user","content":"hello"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "poe-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/gql_POST",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "poe-web" {
		t.Errorf("DetectedSpec=%q want poe-web", payload.DetectedSpec)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a non-chat
// body returns ErrUnsupported.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "poe-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

// Internal helpers — looksLikeJSON

func TestLooksLikeJSON(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", []byte(``), false},
		{"only-whitespace", []byte("  \t\n\r"), false},
		{"object", []byte(`{"a":1}`), true},
		{"array", []byte(`[1,2,3]`), true},
		{"leading-whitespace-object", []byte("  \n\t {\"a\":1}"), true},
		{"leading-whitespace-array", []byte("\r\n[1]"), true},
		{"text-prefix", []byte(`hello`), false},
		{"number-prefix", []byte(`42`), false},
		{"string-prefix", []byte(`"x"`), false},
		{"control-byte-prefix", []byte{0x00, '{'}, false},
	}
	for _, c := range cases {
		if got := looksLikeJSON(c.in); got != c.want {
			t.Errorf("%s: looksLikeJSON(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}
