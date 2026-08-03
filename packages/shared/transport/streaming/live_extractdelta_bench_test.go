package streaming

import "testing"

// Frame fixtures covering the shapes a transparent MITM actually sees. Per
// binding B9 the unknown shapes are the NORMAL case for compliance-proxy and
// agent, not an anomaly — so they are benchmarked as first-class inputs, not
// as an edge case.
var (
	// The one shape the parser is written for.
	frameOpenAIContent = `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1730000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hello "},"finish_reason":null}]}`
	// OpenAI frames that carry no delta text but are still fully parsed today.
	frameOpenAIRole   = `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1730000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
	frameOpenAIFinish = `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1730000000,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	frameOpenAIUsage  = `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1730000000,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":550,"completion_tokens":150,"total_tokens":700}}`
	// Valid JSON, non-OpenAI shape — the B9 case. Parsed in full, yields "".
	frameAnthropic = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`
	// Not JSON at all — the only input that reaches the raw fallback.
	frameRawText = `some non-json provider payload`
)

func benchExtract(b *testing.B, data string) {
	evt := &SSEEvent{Event: "message", Data: data}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = extractDeltaText(evt)
	}
}

// BenchmarkExtractDeltaText_OpenAIContent is the frame the function is built
// for: a full json.Unmarshal that does yield a delta.
func BenchmarkExtractDeltaText_OpenAIContent(b *testing.B) { benchExtract(b, frameOpenAIContent) }

// The next three are OpenAI frames that CANNOT carry delta text. Today each
// pays the same full unmarshal as a content frame. The gw analysis called
// these out by name as cheap-rejectable.
func BenchmarkExtractDeltaText_OpenAIRole(b *testing.B)   { benchExtract(b, frameOpenAIRole) }
func BenchmarkExtractDeltaText_OpenAIFinish(b *testing.B) { benchExtract(b, frameOpenAIFinish) }
func BenchmarkExtractDeltaText_OpenAIUsage(b *testing.B)  { benchExtract(b, frameOpenAIUsage) }

// BenchmarkExtractDeltaText_Anthropic is the binding-B9 case: a valid-JSON
// frame in a shape this function does not model. It pays the full unmarshal
// and returns "".
func BenchmarkExtractDeltaText_Anthropic(b *testing.B) { benchExtract(b, frameAnthropic) }

// BenchmarkExtractDeltaText_RawText is the only input that reaches the
// raw-data fallback, because that branch requires json.Unmarshal to FAIL.
func BenchmarkExtractDeltaText_RawText(b *testing.B) { benchExtract(b, frameRawText) }

// BenchmarkExtractDeltaText_Done pins the cost of the [DONE] sentinel, which
// short-circuits on evt.Done before any parsing — the shape every other frame
// type should aspire to.
func BenchmarkExtractDeltaText_Done(b *testing.B) {
	evt := &SSEEvent{Event: "message", Data: "[DONE]", Done: true}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = extractDeltaText(evt)
	}
}

// BenchmarkExtractDeltaText_Stream150 models one real reply: 150 content
// frames plus the role / finish / usage frames a provider actually emits, so
// the per-frame cost is expressed per REQUEST rather than per call.
func BenchmarkExtractDeltaText_Stream150(b *testing.B) {
	frames := make([]*SSEEvent, 0, 154)
	frames = append(frames, &SSEEvent{Event: "message", Data: frameOpenAIRole})
	for range 150 {
		frames = append(frames, &SSEEvent{Event: "message", Data: frameOpenAIContent})
	}
	frames = append(frames,
		&SSEEvent{Event: "message", Data: frameOpenAIFinish},
		&SSEEvent{Event: "message", Data: frameOpenAIUsage},
		&SSEEvent{Event: "message", Data: "[DONE]", Done: true},
	)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for _, evt := range frames {
			_ = extractDeltaText(evt)
		}
	}
}
