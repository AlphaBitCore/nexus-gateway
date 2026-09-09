package responses

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A reasoning item is the model's own continuation token. Dropping it returns a
// 200 from a model that has forgotten how it got there, and placing it wrongly
// returns a 400 — OpenAI requires a reasoning item to be followed immediately by
// the output item it produced.
//
// Both failures come from the same off-by-one. The decode side numbers carried
// items by how many messages it derived from `input[]`, and a `role:"system"`
// item in `input[]` is one of those. The encode side hoists that system message
// into `instructions` and used to skip it WITHOUT advancing the same counter, so
// every carried item was numbered one slot too high and the last one was never
// reached at all.
//
// The distinguishing case is where the system prompt came from: an
// `instructions` field round-tripping through the extension namespace produces a
// system message the decode SYNTHESISED, which `after` does not count. Both are
// covered here, because a fix that advances the counter unconditionally breaks
// the second one.

func encodeFromWire(t *testing.T, wire string) []byte {
	t.Helper()
	canonical, err := DecodeResponsesRequest([]byte(wire))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func TestSystemInInputKeepsTheReasoningItem(t *testing.T) {
	const wire = `{"model":"gpt-5","input":[
	  {"role":"system","content":"be brief"},
	  {"role":"user","content":"hi"},
	  {"type":"reasoning","id":"rs_1","encrypted_content":"ENC_MUST_SURVIVE"}]}`

	out := encodeFromWire(t, wire)

	if !strings.Contains(string(out), "ENC_MUST_SURVIVE") {
		t.Errorf("the reasoning item was dropped when the system prompt rode in input[].\n"+
			"got: %s\nThe model is handed a conversation it did not have: it answers 200 having "+
			"forgotten the reasoning it was supposed to continue from.", string(out))
	}
	if got := gjson.GetBytes(out, "instructions").Str; got != "be brief" {
		t.Errorf("instructions = %q, want the hoisted system prompt", got)
	}
}

// TestSystemInInputKeepsReasoningBeforeItsOutput pins the ORDER. OpenAI rejects
// a reasoning item that is not immediately followed by the item it produced, so
// a carried item landing one slot late is a 400, not a cosmetic difference.
func TestSystemInInputKeepsReasoningBeforeItsOutput(t *testing.T) {
	const wire = `{"model":"gpt-5","input":[
	  {"role":"system","content":"be brief"},
	  {"role":"user","content":"weather?"},
	  {"type":"reasoning","id":"rs_1","encrypted_content":"ENC"},
	  {"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"sunny"}]}`

	out := encodeFromWire(t, wire)

	var kinds []string
	gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
		if k := item.Get("type").Str; k != "" {
			kinds = append(kinds, k)
		} else {
			kinds = append(kinds, "message:"+item.Get("role").Str)
		}
		return true
	})
	order := strings.Join(kinds, ",")
	const want = "message:user,reasoning,function_call,function_call_output"
	if order != want {
		t.Errorf("input order = %q, want %q.\nfull body: %s\n"+
			"A reasoning item wedged between a call and its output is a 400 from OpenAI: it "+
			"requires the reasoning item to be immediately followed by what it produced.",
			order, want, string(out))
	}
}

// TestInstructionsRoundTripDoesNotShiftCarriedItems is the control. Here the
// system message is the decode's own artifact of hoisting `instructions`, so it
// was never counted by `after` — advancing the counter for it would move every
// carried item one slot early, which is the mirror image of the bug above.
func TestInstructionsRoundTripDoesNotShiftCarriedItems(t *testing.T) {
	const wire = `{"model":"gpt-5","instructions":"be brief","input":[
	  {"role":"user","content":"hi"},
	  {"type":"reasoning","id":"rs_1","encrypted_content":"ENC"}]}`

	out := encodeFromWire(t, wire)

	if !strings.Contains(string(out), "ENC") {
		t.Fatalf("the reasoning item was dropped on the instructions round trip: %s", string(out))
	}
	var kinds []string
	gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
		if k := item.Get("type").Str; k != "" {
			kinds = append(kinds, k)
		} else {
			kinds = append(kinds, "message:"+item.Get("role").Str)
		}
		return true
	})
	order := strings.Join(kinds, ",")
	const want = "message:user,reasoning"
	if order != want {
		t.Errorf("input order = %q, want %q — the carried item moved, so the counter is now "+
			"advancing for a system message that `after` never counted", order, want)
	}
}

// TestAssistantTextThenCallIsOneTurn covers the other half of the same merge
// rule. On the chat wire an assistant that speaks and then calls a tool is ONE
// message carrying both `content` and `tool_calls` — which is exactly what
// OpenAI's own output[] looks like for a reasoning model that narrates before
// it calls.
//
// The merge only fired when the previous message ALREADY had a tool_calls key,
// so a preceding assistant TEXT message opened a second assistant turn instead.
// Two consecutive assistant turns describe a different conversation, and on the
// Anthropic egress they become two consecutive assistant blocks — the very
// argument used to justify merging consecutive calls in the first place, applied
// inconsistently.
func TestAssistantTextThenCallIsOneTurn(t *testing.T) {
	const wire = `{"model":"gpt-5","input":[
	  {"role":"user","content":"weather?"},
	  {"role":"assistant","content":"Let me check."},
	  {"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}]}`

	canonical, err := DecodeResponsesRequest([]byte(wire))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	var roles []string
	gjson.GetBytes(canonical, "messages").ForEach(func(_, m gjson.Result) bool {
		r := m.Get("role").Str
		if m.Get("tool_calls").Exists() {
			r += "+calls"
		}
		if c := m.Get("content"); c.Exists() && c.Str != "" {
			r += "+text"
		}
		roles = append(roles, r)
		return true
	})
	got := strings.Join(roles, ",")
	const want = "user+text,assistant+calls+text"
	if got != want {
		t.Errorf("canonical messages = %q, want %q.\nbody: %s\n"+
			"Speaking and then calling is one assistant turn on the chat wire; splitting it "+
			"produces two consecutive assistant messages, which is a different conversation.",
			got, want, string(canonical))
	}
}

// TestCallResultCallOpensANewTurn is the control the fix must not break: after a
// tool result, the next call genuinely IS a new assistant turn.
func TestCallResultCallOpensANewTurn(t *testing.T) {
	const wire = `{"model":"gpt-5","input":[
	  {"role":"user","content":"weather?"},
	  {"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"sunny"},
	  {"type":"function_call","call_id":"c2","name":"lookup","arguments":"{}"}]}`

	canonical, err := DecodeResponsesRequest([]byte(wire))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var roles []string
	gjson.GetBytes(canonical, "messages").ForEach(func(_, m gjson.Result) bool {
		roles = append(roles, m.Get("role").Str)
		return true
	})
	got := strings.Join(roles, ",")
	const want = "user,assistant,tool,assistant"
	if got != want {
		t.Errorf("roles = %q, want %q — a call after a tool result is a new turn and must not "+
			"be merged backwards", got, want)
	}
}
