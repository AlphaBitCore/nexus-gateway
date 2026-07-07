package canonicalbridge

import (
	"bytes"
	"context"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// goldenEnc returns an openAIStreamEncoder with deterministic id/created so the
// golden byte output is stable (the production constructor seeds id from
// crypto/rand and created from time.Now). These golden frames PIN the exact wire
// bytes the encoder emits; the map[string]any→struct refactor must keep them
// byte-for-byte identical (zero wire change — see plan §12.1).
func goldenEnc() *openAIStreamEncoder {
	return &openAIStreamEncoder{id: "chatcmpl-GOLD", created: 1700000000, model: "test-model"}
}

func gpi(i int) *int { return &i }

// gFrame wraps a choice JSON in the full envelope (alphabetical key order, no usage).
func gFrame(choice string) string {
	return `data: {"choices":[` + choice + `],"created":1700000000,"id":"chatcmpl-GOLD","model":"test-model","object":"chat.completion.chunk"}` + "\n\n"
}

// gFrameU wraps a choice + usage block (usage sorts after object, alphabetically).
func gFrameU(choice, usage string) string {
	return `data: {"choices":[` + choice + `],"created":1700000000,"id":"chatcmpl-GOLD","model":"test-model","object":"chat.completion.chunk","usage":` + usage + `}` + "\n\n"
}

type goldenFrame struct {
	name       string
	headerSent bool
	chunk      provcore.Chunk
	want       string
}

func goldenFrames() []goldenFrame {
	return []goldenFrame{
		{
			name: "role_content", headerSent: false, chunk: provcore.Chunk{Delta: "hello"},
			want: gFrame(`{"delta":{"content":"","role":"assistant"},"finish_reason":null,"index":0}`) +
				gFrame(`{"delta":{"content":"hello"},"finish_reason":null,"index":0}`),
		},
		{
			name: "content", headerSent: true, chunk: provcore.Chunk{Delta: "more"},
			want: gFrame(`{"delta":{"content":"more"},"finish_reason":null,"index":0}`),
		},
		{
			// escape vectors: < > & " \ \n \r \t + unicode/emoji
			name: "content_escape", headerSent: true, chunk: provcore.Chunk{Delta: "a<b>&\"c\\d\n\r\t中😀"},
			want: gFrame(`{"delta":{"content":"` + "a\\u003cb\\u003e\\u0026\\\"c\\\\d\\n\\r\\t中😀" + `"},"finish_reason":null,"index":0}`),
		},
		{
			// SSE frame-injection vector: \n\n must stay inside the JSON string, never split the frame
			name: "content_sse_inject", headerSent: true, chunk: provcore.Chunk{Delta: "data: evil\n\ndata: {\"x\":1}"},
			want: gFrame(`{"delta":{"content":"data: evil\n\ndata: {\"x\":1}"},"finish_reason":null,"index":0}`),
		},
		{
			// JSON breakout vector: quotes escaped, envelope not broken
			name: "content_breakout", headerSent: true, chunk: provcore.Chunk{Delta: "\",\"injected\":\"x"},
			want: gFrame(`{"delta":{"content":"\",\"injected\":\"x"},"finish_reason":null,"index":0}`),
		},
		{
			// XSS vector: angle brackets HTML-safe escaped
			name: "content_xss", headerSent: true, chunk: provcore.Chunk{Delta: "</script><svg onload=alert(1)>"},
			want: gFrame(`{"delta":{"content":"` + "\\u003c/script\\u003e\\u003csvg onload=alert(1)\\u003e" + `"},"finish_reason":null,"index":0}`),
		},
		{
			// control chars escaped as \u00XX; build want via concatenation to avoid
			// embedding real NUL bytes in source.
			name: "content_ctrl", headerSent: true, chunk: provcore.Chunk{Delta: "\x00\x01\x1f"},
			want: gFrame(`{"delta":{"content":"` + "\\u0000\\u0001\\u001f" + `"},"finish_reason":null,"index":0}`),
		},
		{
			name: "toolcall", headerSent: true, chunk: provcore.Chunk{ToolCallDeltas: []provcore.ToolCallDelta{
				{Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{"city":`},
				{Index: 1, Arguments: `"NYC"}`},
			}},
			want: gFrame(`{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}},{"index":1,"function":{"arguments":"\"NYC\"}"}}]},"finish_reason":null,"index":0}`),
		},
		{
			// redacted tool args delivered as a placeholder must survive intact
			name: "toolcall_redact", headerSent: true, chunk: provcore.Chunk{ToolCallDeltas: []provcore.ToolCallDelta{
				{Index: 0, ID: "call_1", Name: "f", Arguments: "[REDACTED]"},
			}},
			want: gFrame(`{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"[REDACTED]"}}]},"finish_reason":null,"index":0}`),
		},
		{
			name: "reasoning", headerSent: true, chunk: provcore.Chunk{ReasoningDelta: "think<>&"},
			want: gFrame(`{"delta":{"reasoning_content":"` + "think\\u003c\\u003e\\u0026" + `"},"finish_reason":null,"index":0}`),
		},
		{
			name: "done_no_usage", headerSent: true, chunk: provcore.Chunk{Done: true},
			want: gFrame(`{"delta":{},"finish_reason":"stop","index":0}`),
		},
		{
			name: "done_usage", headerSent: true, chunk: provcore.Chunk{Done: true, FinishReason: "length", Usage: &provcore.Usage{
				PromptTokens: gpi(10), CompletionTokens: gpi(5), TotalTokens: gpi(15),
			}},
			want: gFrameU(`{"delta":{},"finish_reason":"length","index":0}`, `{"completion_tokens":5,"prompt_tokens":10,"total_tokens":15}`),
		},
		{
			name: "done_usage_details", headerSent: true, chunk: provcore.Chunk{Done: true, Usage: &provcore.Usage{
				PromptTokens: gpi(10), CompletionTokens: gpi(5), TotalTokens: gpi(15),
				CacheReadTokens: gpi(4), ReasoningTokens: gpi(3),
			}},
			want: gFrameU(`{"delta":{},"finish_reason":"stop","index":0}`, `{"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":3},"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":4},"total_tokens":15}`),
		},
		{
			name: "content_done", headerSent: true, chunk: provcore.Chunk{Delta: "tail", Done: true, FinishReason: "stop", Usage: &provcore.Usage{
				PromptTokens: gpi(1), CompletionTokens: gpi(2), TotalTokens: gpi(3),
			}},
			want: gFrame(`{"delta":{"content":"tail"},"finish_reason":null,"index":0}`) +
				gFrameU(`{"delta":{},"finish_reason":"stop","index":0}`, `{"completion_tokens":2,"prompt_tokens":1,"total_tokens":3}`),
		},
	}
}

// TestOAIStreamEncoderGolden pins the exact wire bytes for every frame type and
// escape vector. The map→struct refactor must keep these byte-for-byte identical.
func TestOAIStreamEncoderGolden(t *testing.T) {
	ctx := context.Background()
	for _, f := range goldenFrames() {
		enc := goldenEnc()
		enc.headerSent = f.headerSent
		b, err := enc.Write(ctx, f.chunk)
		if err != nil {
			t.Fatalf("%s: Write: %v", f.name, err)
		}
		if string(b) != f.want {
			t.Errorf("%s byte mismatch:\n got: %q\nwant: %q", f.name, string(b), f.want)
		}
	}
}

// TestOAIStreamEncoderNoResidue proves that reusing one encoder across chunks
// (the scratch-buffer reuse introduced by the refactor) never leaks bytes from a
// prior frame into a later one. A long delta followed by a short delta must
// yield a frame whose content is exactly the short delta — no tail residue.
func TestOAIStreamEncoderNoResidue(t *testing.T) {
	ctx := context.Background()
	enc := goldenEnc()
	enc.headerSent = true

	long, _ := enc.Write(ctx, provcore.Chunk{Delta: strings.Repeat("LONGSECRET", 50)})
	if !bytes.Contains(long, []byte("LONGSECRET")) {
		t.Fatalf("first frame should contain the long delta; got %q", long)
	}
	short, _ := enc.Write(ctx, provcore.Chunk{Delta: "x"})
	if want := gFrame(`{"delta":{"content":"x"},"finish_reason":null,"index":0}`); string(short) != want {
		t.Errorf("second frame leaked prior-frame residue:\n got: %q\nwant: %q", string(short), want)
	}
	if bytes.Contains(short, []byte("LONGSECRET")) {
		t.Errorf("second frame must not contain any byte of the first frame; got %q", short)
	}
}

// TestOAIStreamEncoderCrossRequestIsolation proves two independent encoder
// instances (two requests) never share state — a fresh encoder's first frame is
// the role header, carrying nothing from a previously-used encoder.
func TestOAIStreamEncoderCrossRequestIsolation(t *testing.T) {
	ctx := context.Background()
	encA := goldenEnc()
	_, _ = encA.Write(ctx, provcore.Chunk{Delta: "AAAAA-request-A-secret"})

	encB := goldenEnc()
	b, _ := encB.Write(ctx, provcore.Chunk{Delta: "B"})
	if bytes.Contains(b, []byte("request-A-secret")) {
		t.Errorf("request B leaked request A bytes; got %q", b)
	}
	if !bytes.Contains(b, []byte(`"role":"assistant"`)) {
		t.Errorf("fresh encoder must emit role header first; got %q", b)
	}
}
