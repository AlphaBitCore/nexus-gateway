package codecs

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestOpenAIChat_ToolUseBlockOrderMatchesWire pins the R3 invariant: the
// canonical ContentToolUse blocks the codec emits appear in the SAME order as
// the wire tool_calls[], so ToolCallArgsFromPayload (which walks the blocks)
// stays index-aligned with the wire rewriter (which walks tool_calls[]).
func TestOpenAIChat_ToolUseBlockOrderMatchesWire(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_a","type":"function","function":{"name":"alpha","arguments":"{\"k\":\"va\"}"}},` +
		`{"id":"call_b","type":"function","function":{"name":"beta","arguments":"{\"k\":\"vb\"}"}},` +
		`{"id":"call_c","type":"function","function":{"name":"gamma","arguments":"{\"k\":\"vc\"}"}}` +
		`]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var toolUseCallIDs []string
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentToolUse && b.ToolUse != nil {
				toolUseCallIDs = append(toolUseCallIDs, b.ToolUse.CallID)
			}
		}
	}
	want := []string{"call_a", "call_b", "call_c"} // wire tool_calls[] order
	if len(toolUseCallIDs) != len(want) {
		t.Fatalf("ContentToolUse count=%d want %d (%v)", len(toolUseCallIDs), len(want), toolUseCallIDs)
	}
	for i := range want {
		if toolUseCallIDs[i] != want[i] {
			t.Fatalf("ContentToolUse[%d] callID=%q want %q (order skew vs wire tool_calls[])", i, toolUseCallIDs[i], want[i])
		}
	}

	// The masked args, zipped onto the wire by ToolCallArgsFromPayload + the
	// rewriter, must land on the matching call — assert the args order too.
	args := core.ToolCallArgsFromPayload(got, []core.TransformSpan{{
		Source:         core.SourceHook,
		SourceID:       "x",
		Action:         core.ActionRedact,
		ContentAddress: "messages.0.content.0.toolUse.input.0",
		Start:          0, End: 2, Replacement: "XX",
	}})
	if len(args) != 3 {
		t.Fatalf("ToolCallArgsFromPayload len=%d want 3", len(args))
	}
	// First call's only leaf masked "va"->"XX"; the untouched siblings carry
	// the empty-string sentinel so the rewriter leaves their wire arguments
	// byte-for-byte intact (no clobber). Order still pins the R3 invariant.
	if args[0] != `{"k":"XX"}` || args[1] != "" || args[2] != "" {
		t.Fatalf("args order/masking wrong: %v", args)
	}
}
