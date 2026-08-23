package canonicalbridge

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	anthropicingress "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// One canonical usage, both arms of /v1/messages, the same counters out.
//
// The defect this guards shipped: the non-streaming egress converted canonical
// usage into Anthropic's additive convention while the streaming encoder wrote
// the raw total — and read it off the Done chunk, which for an OpenAI-family
// upstream carries no usage at all. Measured on production, cross-format
// /v1/messages to gpt-4.1:
//
//	non-streaming  input_tokens 2717, output_tokens 1
//	streaming      message_start {input_tokens 0, output_tokens 0}
//	               message_delta {output_tokens 0}
//
// A client billing off the stream saw a free request, and the same request
// answered differently depending on whether it set `stream`. Asserting each arm
// against its own expected numbers would not have caught it — only comparing
// them to EACH OTHER does, which is what this test is.
func TestAnthropicUsage_BothArmsAgree(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		prompt, completion, cacheRead, cacheCreation int
	}{
		{"no cache", 2717, 1, 0, 0},
		{"cache read", 2717, 4, 2402, 0},
		{"cache creation", 12936, 4, 0, 12924},
		{"both cache counters", 5000, 7, 3000, 1500},
		// A provider contradicting itself must contradict itself identically on
		// both arms; whatever is decided about this case has to move together.
		{"counters exceed the total", 100, 2, 90, 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nonStream := nonStreamingCounters(t, tc.prompt, tc.completion, tc.cacheRead, tc.cacheCreation)
			streamed := streamedCounters(t, tc.prompt, tc.completion, tc.cacheRead, tc.cacheCreation)

			for _, field := range []string{
				"input_tokens", "output_tokens",
				"cache_read_input_tokens", "cache_creation_input_tokens",
			} {
				if nonStream[field] != streamed[field] {
					t.Errorf("%s: non-streaming %v, streaming %v — one request, two answers, "+
						"decided by whether the caller set stream",
						field, nonStream[field], streamed[field])
				}
			}
			// And the conversion itself must be the additive one, or both arms
			// agree on the wrong number.
			want := anthropicingress.ToAnthropicCounters(
				int64(tc.prompt), int64(tc.cacheRead), int64(tc.cacheCreation))
			if nonStream["input_tokens"] != want.InputTokens {
				t.Errorf("input_tokens = %v, want %d — Anthropic's input_tokens counts only the "+
					"tokens neither read from nor written to the cache",
					nonStream["input_tokens"], want.InputTokens)
			}
		})
	}
}

// nonStreamingCounters runs the canonical body through the /v1/messages egress.
func nonStreamingCounters(t *testing.T, prompt, completion, cacheRead, cacheCreation int) map[string]int64 {
	t.Helper()
	body := fmt.Sprintf(`{"id":"c","model":"m","choices":[{"index":0,
	  "message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
	  "usage":{"prompt_tokens":%d,"completion_tokens":%d,
	    "prompt_tokens_details":{"cached_tokens":%d,"cache_creation_tokens":%d}}}`,
		prompt, completion, cacheRead, cacheCreation)
	out, err := anthropicingress.OpenAIChatCompletionToMessagesResponse([]byte(body))
	if err != nil {
		t.Fatalf("non-streaming egress: %v", err)
	}
	return countersFrom(gjson.GetBytes(out, "usage"))
}

// streamedCounters drives the transcoding encoder the way a real stream arrives:
// content first, usage in its own late chunk, Done last. That ordering is the
// defect's other half — reading usage off the Done chunk finds nothing.
func streamedCounters(t *testing.T, prompt, completion, cacheRead, cacheCreation int) map[string]int64 {
	t.Helper()
	enc := newAnthropicStreamEncoder()
	var sse strings.Builder

	write := func(c provcore.Chunk) {
		b, err := enc.Write(context.Background(), c)
		if err != nil {
			t.Fatalf("encoder: %v", err)
		}
		sse.Write(b)
	}
	write(provcore.Chunk{Delta: "ok"})
	write(provcore.Chunk{Usage: &normalize.Usage{
		PromptTokens:        &prompt,
		CompletionTokens:    &completion,
		CacheReadTokens:     &cacheRead,
		CacheCreationTokens: &cacheCreation,
	}})
	write(provcore.Chunk{Done: true, FinishReason: "stop"})

	// message_delta is where a real Anthropic upstream puts the authoritative
	// counters — captured from the passthrough leg, which emits
	// {"input_tokens":11,"cache_creation_input_tokens":0,
	//  "cache_read_input_tokens":2402,"output_tokens":4} there.
	var found gjson.Result
	for _, line := range strings.Split(sse.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		ev := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		if ev.Get("type").String() == "message_delta" {
			found = ev.Get("usage")
		}
	}
	if !found.Exists() {
		t.Fatalf("no message_delta usage in the transcoded stream:\n%s", sse.String())
	}
	return countersFrom(found)
}

func countersFrom(u gjson.Result) map[string]int64 {
	out := map[string]int64{}
	for _, f := range []string{
		"input_tokens", "output_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
	} {
		out[f] = u.Get(f).Int()
	}
	return out
}

// The counters have to survive to the END of the stream, not only the event
// that carried them. A client reading message_delta must not depend on usage
// having arrived on that exact chunk.
func TestAnthropicUsage_ArrivesLateAndStillReported(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	prompt, completion := 500, 9
	var sse strings.Builder
	for _, c := range []provcore.Chunk{
		{Delta: "a"},
		{Delta: "b"},
		{Usage: &normalize.Usage{PromptTokens: &prompt, CompletionTokens: &completion}},
		{Done: true, FinishReason: "stop"},
	} {
		b, err := enc.Write(context.Background(), c)
		if err != nil {
			t.Fatalf("encoder: %v", err)
		}
		sse.Write(b)
	}
	var delta gjson.Result
	for _, line := range strings.Split(sse.String(), "\n") {
		if strings.HasPrefix(line, "data:") {
			ev := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if ev.Get("type").String() == "message_delta" {
				delta = ev.Get("usage")
			}
		}
	}
	if got := delta.Get("input_tokens").Int(); got != int64(prompt) {
		t.Errorf("input_tokens = %d, want %d — usage arrived two chunks before Done and the "+
			"encoder has to remember it; reading the Done chunk alone reported zero", got, prompt)
	}
	if got := delta.Get("output_tokens").Int(); got != int64(completion) {
		t.Errorf("output_tokens = %d, want %d", got, completion)
	}
}

// A stream that never reports usage must emit the shape anyway rather than
// omitting the keys — an Anthropic SDK reads them unconditionally.
func TestAnthropicUsage_AbsentUsageStillEmitsTheShape(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	b, err := enc.Write(context.Background(), provcore.Chunk{Done: true, FinishReason: "stop"})
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	var start map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "data:") {
			var ev map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) == nil &&
				ev["type"] == "message_start" {
				start = ev
			}
		}
	}
	if start == nil {
		t.Fatalf("no message_start:\n%s", b)
	}
	u := start["message"].(map[string]any)["usage"].(map[string]any)
	if _, ok := u["input_tokens"]; !ok {
		t.Error("message_start omitted input_tokens; an SDK reading it unconditionally would fail")
	}
	if _, ok := u["output_tokens"]; !ok {
		t.Error("message_start omitted output_tokens")
	}
}
