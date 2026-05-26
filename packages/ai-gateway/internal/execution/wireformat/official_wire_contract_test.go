package wireformat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/tidwall/gjson"
)

// Fixtures are minimal shapes aligned with vendor streaming documentation
// (see package doc in doc.go for URLs). They are not live API responses.

const openAIChatCompletionStreamSSE = "" +
	"data: {\"id\":\"chatcmpl-official\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini-2024-07-18\",\"service_tier\":\"default\",\"system_fingerprint\":\"fp_test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-official\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-official\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n" +
	"data: [DONE]\n\n"

const anthropicMessagesStreamSSE = "" +
	"event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-sonnet-20241022\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
	"event: ping\n" +
	"data: {\"type\":\"ping\"}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":10}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// Leading blank lines before `data:` occur on some Gemini paths (see Google client issues).
const geminiStreamGenerateContentSSE = "" +
	"\n\n" +
	"data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4},\"modelVersion\":\"gemini-2.0-flash\",\"responseId\":\"resp-official-1\"}\n\n" +
	"data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\" there\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":3,\"totalTokenCount\":6},\"modelVersion\":\"gemini-2.0-flash\",\"responseId\":\"resp-official-1\"}\n\n"

func TestOpenAI_OfficialSSE_ChunkValidatorsAndStreamDecoder(t *testing.T) {
	t.Parallel()
	sc := specutil.NewSSEScanner(io.NopCloser(strings.NewReader(openAIChatCompletionStreamSSE)))
	defer sc.Close() //nolint:errcheck
	var sawDone bool
	for {
		ev, err := sc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if IsOpenAIStreamDone(ev.Data) {
			sawDone = true
			continue
		}
		if len(ev.Data) == 0 {
			continue
		}
		if err := ValidateOpenAIChatCompletionChunk(ev.Data); err != nil {
			t.Fatalf("chunk validation: %v\n%s", err, string(ev.Data))
		}
	}
	if !sawDone {
		t.Fatal("expected [DONE] payload")
	}

	dec, err := openai.NewStreamDecoder(slog.Default()).Open(io.NopCloser(strings.NewReader(openAIChatCompletionStreamSSE)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close() //nolint:errcheck
	ctx := context.Background()
	var deltas string
	var sawUsage bool
	for {
		ch, err := dec.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ch.Done {
			continue
		}
		deltas += ch.Delta
		if ch.Usage != nil && ch.Usage.TotalTokens != nil {
			sawUsage = true
		}
	}
	if deltas != "Hello" {
		t.Fatalf("decoder deltas=%q want Hello", deltas)
	}
	if !sawUsage {
		t.Fatal("expected usage on usage chunk")
	}
}

func TestAnthropic_OfficialSSE_ValidatorsAndStreamDecoder(t *testing.T) {
	t.Parallel()
	sc := specutil.NewSSEScanner(io.NopCloser(strings.NewReader(anthropicMessagesStreamSSE)))
	defer sc.Close() //nolint:errcheck
	for {
		ev, err := sc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(ev.Data) == 0 {
			continue
		}
		if err := ValidateAnthropicStreamingJSON(ev.Data); err != nil {
			t.Fatalf("event %q: %v", ev.Event, err)
		}
	}

	dec, err := anthropic.NewStreamDecoder(slog.Default()).Open(io.NopCloser(strings.NewReader(anthropicMessagesStreamSSE)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close() //nolint:errcheck
	ctx := context.Background()
	var text string
	var finished bool
	var sawPrompt25 bool
	for {
		ch, err := dec.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text += ch.Delta
		if ch.Usage != nil && ch.Usage.PromptTokens != nil && *ch.Usage.PromptTokens == 25 {
			sawPrompt25 = true
		}
		if ch.Done {
			finished = true
		}
	}
	if text != "Hi" {
		t.Fatalf("text=%q want Hi", text)
	}
	if !finished {
		t.Fatal("expected message_stop to mark done")
	}
	if !sawPrompt25 {
		t.Fatal("expected message_start usage input_tokens 25 on stream")
	}
}

func TestGemini_OfficialSSE_ValidatorsAndStreamDecoder(t *testing.T) {
	t.Parallel()
	sc := specutil.NewSSEScanner(io.NopCloser(strings.NewReader(geminiStreamGenerateContentSSE)))
	defer sc.Close() //nolint:errcheck
	for {
		ev, err := sc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(ev.Data) == 0 {
			continue
		}
		if err := ValidateGeminiGenerateContentResponseChunk(ev.Data); err != nil {
			t.Fatalf("chunk: %v\n%s", err, string(ev.Data))
		}
	}

	dec, err := gemini.NewStreamDecoder(slog.Default()).Open(io.NopCloser(strings.NewReader(geminiStreamGenerateContentSSE)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close() //nolint:errcheck
	ctx := context.Background()
	var text string
	for {
		ch, err := dec.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text += ch.Delta
	}
	if text != "Hello there" {
		t.Fatalf("text=%q want %q", text, "Hello there")
	}
}

func TestAnthropic_OfficialMessagesRequest_ValidateAndHubCodec(t *testing.T) {
	t.Parallel()
	// Optional Messages fields (metadata, stop_sequences) per Anthropic Messages API.
	native := `{
		"model": "claude-3-5-sonnet-20241022",
		"max_tokens": 256,
		"metadata": {"user_id": "user-abc-123"},
		"stop_sequences": ["</end>"],
		"system": "You are concise.",
		"messages": [{"role": "user", "content": "Say hi."}]
	}`
	if err := ValidateAnthropicMessagesRequest([]byte(native)); err != nil {
		t.Fatal(err)
	}
	out, err := anthropic.MessagesRequestToOpenAIChatCompletion([]byte(native), "claude-3-5-sonnet-20241022")
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(out)
	// Hub maps a single stop_sequence to a string stop (OpenAI-compatible).
	if root.Get("stop").String() != "</end>" {
		t.Fatalf("stop: got %q want </end>", root.Get("stop").String())
	}
	if root.Get("messages.0.role").String() != "system" {
		t.Fatalf("expected merged system as first message")
	}
	if root.Get("messages.0.content").String() != "You are concise." {
		t.Fatalf("system content")
	}
	if root.Get("messages.1.role").String() != "user" {
		t.Fatalf("expected user after system")
	}
}

func TestOpenAI_OfficialChatCompletionRequest_ValidateToolsAndResponseFormat(t *testing.T) {
	t.Parallel()
	req := `{
		"model": "gpt-4o-mini",
		"messages": [{"role": "user", "content": "Return JSON."}],
		"stream": false,
		"tools": [{"type": "function", "function": {"name": "fn", "parameters": {"type": "object", "properties": {}}}}],
		"response_format": {"type": "json_object"}
	}`
	if err := ValidateOpenAIChatCompletionRequest([]byte(req)); err != nil {
		t.Fatal(err)
	}
}

func TestGemini_OfficialGenerateContentRequest_ValidateAndHubCodec(t *testing.T) {
	t.Parallel()
	req := `{
		"contents": [{"role": "user", "parts": [{"text": "Hello"}]}],
		"generationConfig": {"temperature": 0.2, "maxOutputTokens": 128, "topP": 0.9},
		"safetySettings": [{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "BLOCK_ONLY_HIGH"}]
	}`
	if err := ValidateGeminiGenerateContentRequest([]byte(req)); err != nil {
		t.Fatal(err)
	}
	out, err := gemini.GenerateContentRequestToOpenAIChatCompletion([]byte(req), "gemini-2.0-flash")
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(out)
	if root.Get("messages.0.content").String() != "Hello" {
		t.Fatalf("user text: %s", string(out))
	}
	if root.Get("temperature").Float() != 0.2 {
		t.Fatalf("temperature: %v", root.Get("temperature").Float())
	}
	if root.Get("top_p").Float() != 0.9 {
		t.Fatalf("top_p: %v", root.Get("top_p").Float())
	}
	if root.Get("max_tokens").Int() != 128 {
		t.Fatalf("max_tokens: %v", root.Get("max_tokens").Int())
	}
	// safetySettings are not mapped into the hub OpenAI subset; input still validates above.
}
