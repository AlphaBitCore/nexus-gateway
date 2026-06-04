package codec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	apstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func TestCodec_EncodeRequest_toolsAndToolChoice(t *testing.T) {
	canon := []byte(`{
		"model": "claude-3-5-haiku-20240307",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "weather in SF?"}],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "x", "parameters": {"type": "object", "properties": {"loc": {"type": "string"}}}}}],
		"tool_choice": {"type": "function", "function": {"name": "get_weather"}}
	}`)
	var codec Codec
	encRes, err := codec.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(out, "tools.0.name").Exists() || gjson.GetBytes(out, "tools.0.name").String() != "get_weather" {
		t.Fatalf("tools: %s", string(out))
	}
	if gjson.GetBytes(out, "tool_choice.type").String() != "tool" {
		t.Fatalf("tool_choice: %s", string(out))
	}
}

func TestCodec_EncodeRequest_jsonObjectPrefill(t *testing.T) {
	canon := []byte(`{
		"model": "claude-3-5-haiku-20240307",
		"max_tokens": 32,
		"messages": [{"role": "user", "content": "emit json"}],
		"response_format": {"type": "json_object"}
	}`)
	var codec Codec
	encRes, err := codec.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	arr := gjson.GetBytes(out, "messages")
	if !arr.IsArray() || len(arr.Array()) < 2 {
		t.Fatalf("expected json_object prefill assistant message: %s", string(out))
	}
	last := arr.Array()[len(arr.Array())-1]
	if last.Get("role").String() != "assistant" {
		t.Fatal(last.Raw)
	}
}

// TestAnthropicModelMaxOutput pins the per-model fallback values used
// when a caller forwards an OpenAI-shape request that omits max_tokens.
// Anthropic rejects requests without max_tokens (unlike OpenAI which
// treats it as optional), so the codec must synthesize one. The bug
// was a hardcoded 1024 floor that truncated every response from
// callers used to OpenAI's "no cap" default; the fix swaps in
// Anthropic's documented per-family hard max output limit.
func TestAnthropicModelMaxOutput(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5-20251001", 8192},
		{"claude-haiku-4-5", 8192},
		{"claude-sonnet-4-5-20250929", 64000},
		{"claude-sonnet-4-6", 64000},
		{"claude-opus-4-7", 32000},
		{"claude-opus-4-1-20250805", 32000},
		{"claude-3-5-sonnet-20241022", 8192},
		{"claude-3-opus-20240229", 4096},
		{"completely-unknown-model", 8192}, // safe across-Claude floor
	}
	for _, tc := range cases {
		got := AnthropicModelMaxOutput(tc.model)
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.model, got, tc.want)
		}
	}
}

// TestCodec_EncodeRequest_NoMaxTokens_DefaultsByModel verifies the
// EncodeRequest path that callers actually hit: an OpenAI-shape
// request with NO max_tokens AND no max_completion_tokens should emit
// the Anthropic body with max_tokens set to the model's documented
// max output limit (not the legacy 1024 floor).
func TestCodec_EncodeRequest_NoMaxTokens_DefaultsByModel(t *testing.T) {
	canon := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	var c Codec
	encRes, err := c.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 64000 {
		t.Errorf("max_tokens = %d, want 64000 (sonnet-4 family default)", got)
	}
	// The synthesized max_tokens backstop must be observable in Rewrites so
	// the handler stamps x-nexus-coerced and traffic_event / debug surfaces
	// show that the gateway applied a model-default cap.
	wantRewrite := "max_tokens→64000_model_default"
	found := false
	for _, r := range encRes.Rewrites {
		if r == wantRewrite {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Rewrites = %v, want to contain %q", encRes.Rewrites, wantRewrite)
	}
}

// TestCodec_EncodeRequest_jsonSchemaUnsupported pins SDD §2.5 hard rule:
// unmappable canonical fields surface as a structured *provcore.ProviderError
// with type=nexus_field_unsupported, not as a generic fmt.Errorf string.
// TestCodec_EncodeRequest_thinkingPassthrough: an OpenAI-spec
// request carrying nexus.ext.anthropic.thinking has that object forwarded
// verbatim as the outgoing `thinking` field. Malformed extensions are
// dropped with no `thinking` key in the outgoing body.
func TestCodec_EncodeRequest_thinkingPassthrough(t *testing.T) {
	cases := []struct {
		name      string
		canon     string
		wantField bool
		wantType  string // expected value of thinking.type if present
	}{
		{
			name: "valid_object_injected_verbatim",
			canon: `{
				"model": "claude-opus-4-7",
				"max_tokens": 2000,
				"messages": [{"role": "user", "content": "Reason carefully."}],
				"nexus": {"ext": {"anthropic": {"thinking": {"type": "enabled", "budget_tokens": 4096}}}}
			}`,
			wantField: true,
			wantType:  "enabled",
		},
		{
			name: "no_extension_no_field",
			canon: `{
				"model": "claude-opus-4-7",
				"max_tokens": 2000,
				"messages": [{"role": "user", "content": "Plain ask."}]
			}`,
			wantField: false,
		},
		{
			name: "malformed_string_dropped",
			canon: `{
				"model": "claude-opus-4-7",
				"max_tokens": 2000,
				"messages": [{"role": "user", "content": "Plain ask."}],
				"nexus": {"ext": {"anthropic": {"thinking": "invalid-string"}}}
			}`,
			wantField: false,
		},
		{
			name: "empty_object_dropped",
			canon: `{
				"model": "claude-opus-4-7",
				"max_tokens": 2000,
				"messages": [{"role": "user", "content": "Plain ask."}],
				"nexus": {"ext": {"anthropic": {"thinking": {}}}}
			}`,
			wantField: false,
		},
	}
	var c Codec
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encRes, err := c.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(tc.canon), provcore.CallTarget{})
			out := encRes.Body
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			got := gjson.GetBytes(out, "thinking")
			if tc.wantField {
				if !got.Exists() {
					t.Fatalf("thinking field missing; body=%s", string(out))
				}
				if gjson.GetBytes(out, "thinking.type").String() != tc.wantType {
					t.Errorf("thinking.type = %q, want %q", gjson.GetBytes(out, "thinking.type").String(), tc.wantType)
				}
			} else if got.Exists() {
				t.Errorf("thinking field should not be present, got %s", got.Raw)
			}
		})
	}
}

func TestCodec_EncodeRequest_jsonSchemaUnsupported(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-haiku-20240307",
		"max_tokens":32,
		"messages":[{"role":"user","content":"json me"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}
	}`)
	var codec Codec
	_, err := codec.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code=%q want %q", pe.Code, provcore.CodeInvalidRequest)
	}
	if pe.Type != "nexus_field_unsupported" {
		t.Errorf("type=%q want nexus_field_unsupported", pe.Type)
	}
	if pe.Status != http.StatusBadRequest {
		t.Errorf("status=%d want 400", pe.Status)
	}
	if !strings.Contains(pe.Message, "response_format.json_schema") {
		t.Errorf("message missing field name: %q", pe.Message)
	}
}

func TestHub_nativeAssistantToolUse_toCanonical_toWire(t *testing.T) {
	native := []byte(`{
		"model": "claude-3-5-haiku-20240307",
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "call tool"}]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "tu_1", "name": "noop", "input": {"a": 1}}
			]}
		]
	}`)
	canon, err := ingress.MessagesRequestToOpenAIChatCompletion(native, "")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(canon, "messages.1.role").String() != "assistant" {
		t.Fatalf("role: %s", string(canon))
	}
	if !gjson.GetBytes(canon, "messages.1.tool_calls").Exists() {
		t.Fatalf("missing tool_calls: %s", string(canon))
	}
	var cdc Codec
	encRes, err := cdc.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	wire := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(wire, "messages.1.content.0.type").String() != "tool_use" {
		t.Fatalf("wire: %s", string(wire))
	}
}

// TestRoundTrip_threeTurnToolUse_AnthropicNative covers the full SDD T-ANT-TOOLS
// acceptance: user → assistant tool_use → tool_result → assistant final, ingested
// natively as Anthropic Messages and then re-emitted as Anthropic wire bytes via
// canonical OpenAI. Every semantic field from the four turns must survive.
func TestRoundTrip_threeTurnToolUse_AnthropicNative(t *testing.T) {
	native, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "execution", "canonicalbridge", "testdata", "anthropic_chat_tooluse.native.json"))
	if err != nil {
		t.Fatal(err)
	}
	canon, err := ingress.MessagesRequestToOpenAIChatCompletion(native, "")
	if err != nil {
		t.Fatalf("hub_ingress: %v", err)
	}

	if got := gjson.GetBytes(canon, "messages.0.role").String(); got != "system" {
		t.Errorf("messages.0.role=%q want system (Anthropic top-level system → canonical leading system message)", got)
	}
	if !gjson.GetBytes(canon, "messages.1.role").Exists() {
		t.Fatalf("missing user turn: %s", string(canon))
	}
	if got := gjson.GetBytes(canon, "messages.2.tool_calls.0.function.name").String(); got != "get_weather" {
		t.Errorf("assistant tool_use lost: tool_calls[0].name=%q want get_weather", got)
	}
	if got := gjson.GetBytes(canon, "messages.2.tool_calls.0.id").String(); got != "toolu_01ABC" {
		t.Errorf("tool_call id lost: %q", got)
	}
	if got := gjson.GetBytes(canon, "messages.3.role").String(); got != "tool" {
		t.Errorf("tool_result must canonicalise to role=tool, got %q", got)
	}
	if got := gjson.GetBytes(canon, "messages.3.tool_call_id").String(); got != "toolu_01ABC" {
		t.Errorf("tool_call_id lost: %q", got)
	}
	if got := gjson.GetBytes(canon, "messages.4.role").String(); got != "assistant" {
		t.Errorf("final assistant turn lost: role=%q", got)
	}

	var cdc Codec
	encRes, err := cdc.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	wire := encRes.Body
	if err != nil {
		t.Fatalf("encode back: %v", err)
	}
	if got := gjson.GetBytes(wire, "system").String(); got != "You are a helpful weather assistant." {
		t.Errorf("system prompt lost on encode: %q", got)
	}
	if got := gjson.GetBytes(wire, "messages.1.content.0.type").String(); got != "tool_use" {
		t.Errorf("assistant tool_use lost on encode: type=%q", got)
	}
	if got := gjson.GetBytes(wire, "messages.1.content.0.id").String(); got != "toolu_01ABC" {
		t.Errorf("tool_use id lost on encode: %q", got)
	}
	if got := gjson.GetBytes(wire, "messages.2.content.0.type").String(); got != "tool_result" {
		t.Errorf("tool_result lost on encode: type=%q", got)
	}
	if got := gjson.GetBytes(wire, "messages.2.content.0.tool_use_id").String(); got != "toolu_01ABC" {
		t.Errorf("tool_use_id lost on encode: %q", got)
	}
	if got := gjson.GetBytes(wire, "tools.0.name").String(); got != "get_weather" {
		t.Errorf("tool list lost: %q", got)
	}
}

// TestEncodeRequest_imageURLs covers SDD T-ANT-MULTIMODAL: canonical OpenAI
// image_url parts (URL + base64 data URL) must produce Anthropic native
// image source blocks of type url and type base64 respectively.
func TestEncodeRequest_imageURLs(t *testing.T) {
	t.Run("https URL", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":32,
			"messages":[{"role":"user","content":[
				{"type":"text","text":"describe"},
				{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"auto"}}
			]}]
		}`)
		var cdc Codec
		encRes, err := cdc.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		wire := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(wire, "messages.0.content.1.type").String(); got != "image" {
			t.Fatalf("image part type=%q want image: %s", got, string(wire))
		}
		if got := gjson.GetBytes(wire, "messages.0.content.1.source.type").String(); got != "url" {
			t.Errorf("source.type=%q want url", got)
		}
		if got := gjson.GetBytes(wire, "messages.0.content.1.source.url").String(); got != "https://example.com/cat.png" {
			t.Errorf("source.url=%q", got)
		}
	})

	t.Run("base64 data URL", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":32,
			"messages":[{"role":"user","content":[
				{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8=","detail":"auto"}}
			]}]
		}`)
		var cdc Codec
		encRes, err := cdc.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		wire := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(wire, "messages.0.content.0.source.type").String(); got != "base64" {
			t.Errorf("source.type=%q want base64", got)
		}
		if got := gjson.GetBytes(wire, "messages.0.content.0.source.media_type").String(); got != "image/png" {
			t.Errorf("media_type=%q want image/png", got)
		}
		if got := gjson.GetBytes(wire, "messages.0.content.0.source.data").String(); got != "aGVsbG8=" {
			t.Errorf("data=%q", got)
		}
	})

	t.Run("detail=high rejected with structured error", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":32,
			"messages":[{"role":"user","content":[
				{"type":"image_url","image_url":{"url":"https://example.com/x.png","detail":"high"}}
			]}]
		}`)
		var cdc Codec
		_, err := cdc.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		if err == nil {
			t.Fatal("expected error")
		}
		var pe *provcore.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("expected *provcore.ProviderError: %T %v", err, err)
		}
		if pe.Type != "nexus_field_unsupported" {
			t.Errorf("type=%q", pe.Type)
		}
	})
}

// TestRoundTrip_imageHTTPSURL_AnthropicNative covers the reverse direction:
// a native Anthropic image (source.type=url) ingested through hub_ingress
// must map to canonical image_url with the same URL. Round-tripped back
// through codec.EncodeRequest the URL must survive verbatim.
func TestRoundTrip_imageHTTPSURL_AnthropicNative(t *testing.T) {
	native := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":32,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}
		]}]
	}`)
	canon, err := ingress.MessagesRequestToOpenAIChatCompletion(native, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(canon, "messages.0.content.1.image_url.url").String(); got != "https://example.com/cat.png" {
		t.Fatalf("hub_ingress lost image URL: %s", string(canon))
	}
	var cdc Codec
	encRes, err := cdc.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	wire := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(wire, "messages.0.content.1.source.url").String(); got != "https://example.com/cat.png" {
		t.Fatalf("encode lost image URL: %s", string(wire))
	}
}

// TestStreamDecoder_PromptTokensFromMessageStart covers SDD T-ANT-STREAM-1:
// the message_start event's message.usage.input_tokens must populate
// chunk.Usage.PromptTokens on the first chunk so quota reconcile sees them.
func TestStreamDecoder_PromptTokensFromMessageStart(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":25,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	dec := apstream.NewStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var firstUsage *provcore.Usage
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if firstUsage == nil && ch.Usage != nil {
			firstUsage = ch.Usage
		}
	}
	if firstUsage == nil {
		t.Fatal("no usage emitted; message_start.input_tokens lost")
	}
	if firstUsage.PromptTokens == nil || *firstUsage.PromptTokens != 25 {
		t.Fatalf("PromptTokens=%v want 25", firstUsage.PromptTokens)
	}
}

// TestEncodeRequest_ParallelToolCalls pins the audit fix: canonical
// parallel_tool_calls=false must surface as Anthropic
// tool_choice.disable_parallel_tool_use=true (inverted boolean), NOT a
// top-level parallel_tool_calls field — Anthropic ignores the latter.
func TestEncodeRequest_ParallelToolCalls(t *testing.T) {
	t.Run("disable_with_existing_tool_choice", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":32,
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
			"tool_choice":"required",
			"parallel_tool_calls":false
		}`)
		encRes, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "parallel_tool_calls").Exists() {
			t.Errorf("top-level parallel_tool_calls leaked: %s", string(out))
		}
		if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "any" {
			t.Errorf("tool_choice.type=%q want any", got)
		}
		if !gjson.GetBytes(out, "tool_choice.disable_parallel_tool_use").Bool() {
			t.Errorf("disable_parallel_tool_use missing: %s", string(out))
		}
	})

	t.Run("disable_without_tool_choice_defaults_to_auto", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":32,
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
			"parallel_tool_calls":false
		}`)
		encRes, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "auto" {
			t.Errorf("tool_choice.type=%q want auto", got)
		}
		if !gjson.GetBytes(out, "tool_choice.disable_parallel_tool_use").Bool() {
			t.Errorf("disable_parallel_tool_use missing: %s", string(out))
		}
	})

	t.Run("enabled_no_disable_field", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":32,
			"messages":[{"role":"user","content":"hi"}],
			"parallel_tool_calls":true
		}`)
		encRes, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "tool_choice.disable_parallel_tool_use").Exists() {
			t.Errorf("disable_parallel_tool_use must be absent when parallel_tool_calls=true: %s", string(out))
		}
	})
}

// TestStreamDecoder_ContentBlockStopClearsToolState pins that content_block_stop
// frees the per-block tool slot so a subsequent tool_use block reusing the
// same index does not inherit the previous tool's id/name.
func TestStreamDecoder_ContentBlockStopClearsToolState(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_first","name":"f"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		// Reuse index 0 for a brand-new tool — old state must NOT leak.
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_second","name":"g"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	dec := apstream.NewStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var seen []provcore.ToolCallDelta
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		seen = append(seen, ch.ToolCallDeltas...)
	}
	// First delta uses toolu_first; second delta (after content_block_stop)
	// must report toolu_second / g — proving the slot was cleared.
	var sawSecond bool
	for _, d := range seen {
		if d.ID == "toolu_second" && d.Name == "g" {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Fatalf("second tool not picked up after content_block_stop: %#v", seen)
	}
}

// TestStreamDecoder_ErrorEventSurfacesProviderError covers the audit's
// "stream error swallowed" finding: an `event: error` frame must produce
// a typed *provcore.ProviderError, not look like a successful end.
func TestStreamDecoder_ErrorEventSurfacesProviderError(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"upstream is at capacity"}}`,
		``,
	}, "\n")
	dec := apstream.NewStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var lastErr error
	for {
		_, err := sess.Next(context.Background())
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected error from Next, got nil")
	}
	var pe *provcore.ProviderError
	if !errors.As(lastErr, &pe) {
		t.Fatalf("expected *provcore.ProviderError, got %T: %v", lastErr, lastErr)
	}
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code=%q want rate_limited", pe.Code)
	}
	if pe.Type != "overloaded_error" {
		t.Errorf("type=%q want overloaded_error", pe.Type)
	}
}

// TestStreamDecoder_ThinkingDelta covers the audit finding that
// extended-thinking content was silently lost on canonical Delta. It must
// land on chunk.ReasoningDelta so audit / hooks aggregating Delta still
// see only assistant-visible content.
func TestStreamDecoder_ThinkingDelta(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_abc"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	dec := apstream.NewStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var content, reasoning string
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		content += ch.Delta
		reasoning += ch.ReasoningDelta
	}
	if content != "Hi" {
		t.Errorf("Delta=%q want Hi (thinking must NOT mix into Delta)", content)
	}
	if !strings.Contains(reasoning, "Let me think") {
		t.Errorf("ReasoningDelta missing thinking_delta text: %q", reasoning)
	}
	if !strings.Contains(reasoning, "sig_abc") {
		t.Errorf("ReasoningDelta missing signature_delta: %q", reasoning)
	}
}

// TestEncodeRequest_UnsupportedFieldWARN pins SDD §2.5 / §7.7: codec drops
// canonical fields it cannot map (e.g. logit_bias on Anthropic) but emits a
// process-deduped WARN so the operator sees the drift exactly once.
func TestEncodeRequest_UnsupportedFieldWARN(t *testing.T) {
	canonicalext.ResetWarnSeenForTest()
	defer canonicalext.ResetWarnSeenForTest()
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	canon := []byte(`{
		"model":"claude-3-5-haiku-20240307",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hi"}],
		"logit_bias":{"50256":-100},
		"seed":42
	}`)
	var cdc Codec
	for range 3 {
		if _, err := cdc.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{}); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.String()
	if c := strings.Count(out, "field=logit_bias"); c != 1 {
		t.Errorf("expected 1 logit_bias WARN, got %d:\n%s", c, out)
	}
	if c := strings.Count(out, "field=seed"); c != 1 {
		t.Errorf("expected 1 seed WARN, got %d", c)
	}
	if c := strings.Count(out, "field=model"); c != 0 {
		t.Errorf("supported field model must not warn:\n%s", out)
	}
}

// TestDecodeResponse_PromptCacheUsage covers SDD T-USAGE-EXT for Anthropic:
// cache_read_input_tokens lands on canonical
// usage.prompt_tokens_details.cached_tokens AND on the typed Usage envelope
// (CacheReadTokens) so cost analytics see the cache split without re-parsing
// the canonical body. cache_creation_input_tokens lands on
// nexus.ext.anthropic.cache_creation_input_tokens.
func TestDecodeResponse_PromptCacheUsage(t *testing.T) {
	native, err := os.ReadFile(filepath.Join("..", "testdata", "prompt_cache_response.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cdc Codec
	decRes, err := cdc.DecodeResponse(typology.WireShapeAnthropicMessages, native, "")
	canon := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(canon, "usage.prompt_tokens_details.cached_tokens").Int(); got != 384 {
		t.Errorf("cached_tokens=%d want 384", got)
	}
	if got := gjson.GetBytes(canon, "nexus.ext.anthropic.cache_creation_input_tokens").Int(); got != 1024 {
		t.Errorf("nexus.ext.anthropic.cache_creation_input_tokens=%d want 1024", got)
	}
	if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 384 {
		t.Errorf("typed Usage.CacheReadTokens=%v want 384", usage.CacheReadTokens)
	}
}

// TestCodec_EncodeRequest_StripsSamplingParamsForClaudeOpus47 verifies
// that the codec strips temperature / top_p / top_k when the target
// model is claude-opus-4-7, returns rewrites in the second return value
// for the x-nexus-coerced header, and leaves all other Anthropic
// models untouched. Reason: api.anthropic.com returns
// 400 "`temperature` is deprecated for this model." for the 4.7 family
// (observed in prod traffic d914275a-0dae-4d13-a811-69e4d432c441).
func TestCodec_EncodeRequest_StripsSamplingParamsForClaudeOpus47(t *testing.T) {
	canon := []byte(`{
		"model": "claude-opus-4-7",
		"messages": [{"role":"user","content":"hi"}],
		"temperature": 0.3,
		"top_p": 0.9,
		"top_k": 40
	}`)
	encRes, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{ProviderModelID: "claude-opus-4-7"})
	body := encRes.Body
	rewrites := encRes.Rewrites
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if gjson.GetBytes(body, "temperature").Exists() {
		t.Errorf("temperature not stripped: %s", string(body))
	}
	if gjson.GetBytes(body, "top_p").Exists() {
		t.Errorf("top_p not stripped: %s", string(body))
	}
	if gjson.GetBytes(body, "top_k").Exists() {
		t.Errorf("top_k not stripped: %s", string(body))
	}
	wantRewrites := map[string]bool{
		"temperature→removed": false,
		"top_p→removed":       false,
		"top_k→removed":       false,
	}
	for _, r := range rewrites {
		if _, ok := wantRewrites[r]; ok {
			wantRewrites[r] = true
		}
	}
	for k, seen := range wantRewrites {
		if !seen {
			t.Errorf("rewrite %q missing from %v", k, rewrites)
		}
	}
}

// TestCodec_EncodeRequest_KeepsSamplingParamsForOtherModels asserts the
// strip is targeted: older Anthropic families (opus-4-1, sonnet-4-6,
// haiku-4-5, 3.x) still accept temperature OR top_p alone, so the codec
// must keep forwarding the single-param case unchanged. The both-set
// case is exercised by TestCodec_EncodeRequest_Drops4xTopPWhenTemperaturePresent.
func TestCodec_EncodeRequest_KeepsSamplingParamsForOtherModels(t *testing.T) {
	models := []string{
		"claude-opus-4-1-20250805",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
		"claude-3-5-sonnet-20241022",
		"claude-3-opus-20240229",
	}
	for _, m := range models {
		t.Run(m+"/temperature-only", func(t *testing.T) {
			canon := []byte(`{
				"model": "` + m + `",
				"messages": [{"role":"user","content":"hi"}],
				"temperature": 0.5
			}`)
			encRes, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{ProviderModelID: m})
			body := encRes.Body
			rewrites := encRes.Rewrites
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if v := gjson.GetBytes(body, "temperature").Float(); v != 0.5 {
				t.Errorf("temperature=%v want 0.5; body=%s", v, string(body))
			}
			for _, r := range rewrites {
				if strings.HasSuffix(r, "→removed") {
					t.Errorf("unexpected strip rewrite for %s: %q", m, r)
				}
			}
		})
		t.Run(m+"/top_p-only", func(t *testing.T) {
			canon := []byte(`{
				"model": "` + m + `",
				"messages": [{"role":"user","content":"hi"}],
				"top_p": 0.95
			}`)
			encRes2, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{ProviderModelID: m})
			body := encRes2.Body
			rewrites := encRes2.Rewrites
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if v := gjson.GetBytes(body, "top_p").Float(); v != 0.95 {
				t.Errorf("top_p=%v want 0.95; body=%s", v, string(body))
			}
			for _, r := range rewrites {
				if strings.HasSuffix(r, "→removed") || strings.Contains(r, "removed_with") {
					t.Errorf("unexpected strip rewrite for %s: %q", m, r)
				}
			}
		})
	}
}

// TestCodec_EncodeRequest_Drops4xTopPWhenTemperaturePresent verifies the
// claude-4.x "either-or" rule: every 4.x family (except 4-7) accepts
// temperature or top_p alone but rejects the combination. When the
// caller sends both, drop top_p, keep temperature, surface the rewrite.
// 3.x stays untouched.
func TestCodec_EncodeRequest_Drops4xTopPWhenTemperaturePresent(t *testing.T) {
	cases := []struct {
		model       string
		wantTopP    bool
		wantRewrite bool
	}{
		{"claude-haiku-4-5-20251001", false, true},
		{"claude-opus-4-1-20250805", false, true},
		{"claude-opus-4-5-20250929", false, true},
		{"claude-opus-4-6", false, true},
		{"claude-sonnet-4-5-20250929", false, true},
		{"claude-sonnet-4-6", false, true},
		{"claude-3-5-sonnet-20241022", true, false},
		{"claude-3-opus-20240229", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			canon := []byte(`{
				"model": "` + tc.model + `",
				"messages": [{"role":"user","content":"hi"}],
				"temperature": 0.3,
				"top_p": 0.9
			}`)
			encRes3, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{ProviderModelID: tc.model})
			body := encRes3.Body
			rewrites := encRes3.Rewrites
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := gjson.GetBytes(body, "temperature").Float(); got != 0.3 {
				t.Errorf("temperature=%v want 0.3 (must always survive)", got)
			}
			hasTopP := gjson.GetBytes(body, "top_p").Exists()
			if hasTopP != tc.wantTopP {
				t.Errorf("top_p present=%v want %v body=%s", hasTopP, tc.wantTopP, body)
			}
			sawRewrite := false
			for _, r := range rewrites {
				if r == "top_p→removed_with_temperature_present" {
					sawRewrite = true
				}
			}
			if sawRewrite != tc.wantRewrite {
				t.Errorf("rewrite present=%v want %v (rewrites=%v)", sawRewrite, tc.wantRewrite, rewrites)
			}
		})
	}
}

// TestCodec_EncodeRequest_4xKeepsTopPAlone asserts the same 4.x rule
// is conditional on BOTH being set. Caller sending only top_p (or only
// temperature) must reach the upstream untouched — single-param is
// accepted by every claude-4.x model.
func TestCodec_EncodeRequest_4xKeepsTopPAlone(t *testing.T) {
	canon := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{"role":"user","content":"hi"}],
		"top_p": 0.9
	}`)
	encRes4, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{ProviderModelID: "claude-sonnet-4-6"})
	body := encRes4.Body
	rewrites := encRes4.Rewrites
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(body, "top_p").Float(); got != 0.9 {
		t.Errorf("top_p=%v want 0.9 (single-param must pass through)", got)
	}
	for _, r := range rewrites {
		if strings.Contains(r, "top_p") {
			t.Errorf("unexpected top_p rewrite on single-param: %q", r)
		}
	}
}

// TestAnthropicModelRejectsTempTopPTogether pins the prefix-list for
// the either-or rule. Adds future-4.x and 3.x as negative anchors so
// drift surfaces a clear test failure.
func TestAnthropicModelRejectsTempTopPTogether(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-haiku-4-5", true},
		{"claude-haiku-4-5-20251001", true},
		{"claude-opus-4-1", true},
		{"claude-opus-4-1-20250805", true},
		{"claude-opus-4-5", true},
		{"claude-opus-4-6", true},
		{"claude-opus-4-7", true}, // also true (covered separately by single-param rule)
		{"claude-sonnet-4-5", true},
		{"claude-sonnet-4-5-20250929", true},
		{"claude-sonnet-4-6", true},
		{"claude-3-5-sonnet-20241022", false},
		{"claude-3-opus-20240229", false},
		{"claude-3-haiku-20240307", false},
		{"", false},
		{"completely-unknown-model", false},
	}
	for _, tc := range cases {
		got := anthropicModelRejectsTempTopPTogether(tc.model)
		if got != tc.want {
			t.Errorf("anthropicModelRejectsTempTopPTogether(%q)=%v want %v", tc.model, got, tc.want)
		}
	}
}

// TestAnthropicModelRejectsSamplingParams pins the prefix-list. New
// observed-rejecting families must extend the switch in codec.go and
// add a case here so the policy is reviewable in one place.
func TestAnthropicModelRejectsSamplingParams(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-opus-4-7", true},
		{"claude-opus-4-7-20260101", true}, // dated variant in the same family
		{"claude-opus-4-1-20250805", false},
		{"claude-opus-4-5-20250929", false}, // not yet observed; needs empirical confirmation
		{"claude-sonnet-4-6", false},
		{"claude-haiku-4-5", false},
		{"claude-3-5-sonnet-20241022", false},
		{"", false},
		{"completely-unknown-model", false},
	}
	for _, tc := range cases {
		got := anthropicModelRejectsSamplingParams(tc.model)
		if got != tc.want {
			t.Errorf("anthropicModelRejectsSamplingParams(%q)=%v want %v", tc.model, got, tc.want)
		}
	}
}
