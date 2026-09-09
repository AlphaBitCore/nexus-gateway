// Package cohere — the reasoning block a Cohere reasoning model answers with.
// Named failure modes:
//   - the thinking block never reaches canonical (no rule can scan it, no egress renders it)
//   - the thinking text is folded into content (private reasoning shown as the answer)
//   - a tool plan and a thinking block both present: the plan must keep precedence
package cohere

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func TestDecodeResponse_ReasoningBlockReachesCanonical(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		wantContent   string
		wantReasoning string
	}{
		{
			// command-a-reasoning answers with two blocks. Only the text block is
			// the answer; the thinking block is the model's private reasoning and
			// belongs on the reasoning channel, not in content.
			name: "thinking block beside the answer",
			body: `{"id":"r1","message":{"role":"assistant","content":[
				{"type":"thinking","thinking":"17 minus 8 leaves 9."},
				{"type":"text","text":"9 sheep."}]},"finish_reason":"COMPLETE"}`,
			wantContent:   "9 sheep.",
			wantReasoning: "17 minus 8 leaves 9.",
		},
		{
			// A tool-calling turn carries a plan and no thinking block; the plan
			// is the reasoning channel there.
			name: "tool plan only",
			body: `{"id":"r2","message":{"role":"assistant","tool_plan":"I will call get_weather.",
				"tool_calls":[{"id":"t1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
				"finish_reason":"TOOL_CALL"}`,
			wantContent:   "",
			wantReasoning: "I will call get_weather.",
		},
		{
			// Both present: the plan wins, so a turn cannot report two different
			// reasoning traces on one canonical field.
			name: "plan takes precedence over thinking",
			body: `{"id":"r3","message":{"role":"assistant","tool_plan":"plan wins",
				"content":[{"type":"thinking","thinking":"thinking loses"},{"type":"text","text":"answer"}]},
				"finish_reason":"COMPLETE"}`,
			wantContent:   "answer",
			wantReasoning: "plan wins",
		},
		{
			name:          "no reasoning at all leaves the field absent",
			body:          `{"id":"r4","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"COMPLETE"}`,
			wantContent:   "hi",
			wantReasoning: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := codec{}.DecodeResponse(typology.WireShapeCohereChat, []byte(tc.body), "",
				provcore.DecodeContext{})
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			msg := gjson.GetBytes(res.CanonicalBody, "choices.0.message")
			if got := msg.Get("content").String(); got != tc.wantContent {
				t.Errorf("content = %q, want %q", got, tc.wantContent)
			}
			got := msg.Get("reasoning_content")
			if tc.wantReasoning == "" {
				if got.Exists() {
					t.Errorf("reasoning_content = %q, want the field absent", got.String())
				}
				return
			}
			if got.String() != tc.wantReasoning {
				t.Errorf("reasoning_content = %q, want %q", got.String(), tc.wantReasoning)
			}
		})
	}
}
