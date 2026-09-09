package codecs

// The OpenAI shortcut in ExtractUsageOnly skips projecting choices[] when the
// body can carry no reasoning text. These cases exist because the existing
// differential table was written for the path that always projected, so none
// of its bodies sit either side of the boundary the shortcut introduces.
//
// The one that matters is the escaped spelling. `"\u0072easoning_content"`
// parses to the same key as the literal, so a substring test alone would take
// the shortcut and silently drop the estimate — and ReasoningTokens is
// billing-adjacent. The backslash guard is what makes the shortcut exact, and
// escaped_reasoning_content_key is the case that fails without it.
//
// The oracle is extractOpenAIUsageProjected itself rather than a
// reimplementation: a second copy of the projection would be free to drift
// from the one it is supposed to prove.

import (
	"reflect"
	"testing"
)

func TestExtractUsageOnly_ShortcutMatchesProjectedPath(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	cases := []struct {
		name string
		body string
	}{{
		// The ordinary case the shortcut exists for.
		name: "plain_response_no_reasoning",
		body: `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`,
	}, {
		// No usage block and no reasoning: nil, not an invented estimate.
		name: "no_usage_no_reasoning",
		body: `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
	}, {
		// A backslash with no reasoning sends this down the projected path;
		// the answer must be the same either way.
		name: "escaped_content_no_reasoning",
		body: `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"say \"hi\""}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
	}, {
		// The word in visible content, not as a key: the shortcut declines and
		// costs nothing but the old path.
		name: "reasoning_word_in_visible_content",
		body: `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"my reasoning is simple"}}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`,
	}, {
		name: "literal_reasoning_content_key",
		body: `{"model":"kimi-k2","choices":[{"message":{"role":"assistant","content":"42","reasoning_content":"a fairly long deliberation here"}}],"usage":{"prompt_tokens":30,"completion_tokens":50,"total_tokens":80}}`,
	}, {
		// THE ONE THE GUARD IS FOR — the key is written with an escape.
		name: "escaped_reasoning_content_key",
		body: "{\"model\":\"kimi-k2\",\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"42\"," +
			"\"\\u0072easoning_content\":\"a fairly long deliberation here\"}}]," +
			"\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":50,\"total_tokens\":80}}",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := n.ExtractUsageOnly([]byte(c.body))
			want, wantOK := extractOpenAIUsageProjected([]byte(c.body))
			if ok != wantOK {
				t.Fatalf("acceptance differs: shortcut=%v projected=%v", ok, wantOK)
			}
			if !reflect.DeepEqual(got, want) {
				// Pointer fields print as addresses; the body is what makes a
				// failure diagnosable, and DeepEqual has already named the case.
				t.Fatalf("shortcut and projected path disagree on %s\n  body: %s", c.name, c.body)
			}
		})
	}
}
