// Package responses — the multi-turn tool conversation across the request waist.
//
// Named failure modes:
//   - an assistant tool call echoed in input[] does not reach canonical, so the
//     tool result that follows names a call nobody declared
//   - an assistant tool call in canonical does not reach the Responses wire
//   - an input item this codec does not model becomes a null-content user turn,
//     which every upstream rejects
//   - a reasoning item's encrypted_content does not survive, so the model loses
//     the chain it was told to continue
package responses

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

// The fixtures are REQUESTS OPENAI ANSWERED 200 FOR, captured by
// testdata/upstream-requests/capture.py. Turn 2's input[] replays turn 1's
// output items verbatim — item ids, call_id spelling, status fields, the
// encrypted reasoning blob — so what this file asserts against is OpenAI's own
// grammar rather than a reconstruction of it. Hand-writing the fixture is how
// the first draft of this test nearly shipped: it would have proved the codec
// handles the shape I imagined, on the one endpoint whose input grammar is the
// least like the chat one.
const requestCorpusDir = "../../../../execution/canonicalbridge/testdata/upstream-requests"

// TestEmitRoundTripWire is not an assertion — it is the first half of the
// two-step flow that refreshes the upstream-accepted goldens, kept in the repo
// so the next person does not have to reinvent it:
//
//	NEXUS_EMIT_ROUNDTRIP=/tmp/rt go test -run TestEmitRoundTripWire ./...
//	python3 <corpus>/capture.py verify <name> /tmp/rt_<name>.json
//
// The second command sends what this wrote to OpenAI and stores the pair only
// if the provider accepts it, which is what makes the golden evidence rather
// than a snapshot of current behaviour. Without the env var it does nothing.
func TestEmitRoundTripWire(t *testing.T) {
	prefix := os.Getenv("NEXUS_EMIT_ROUNDTRIP")
	if prefix == "" {
		t.Skip("set NEXUS_EMIT_ROUNDTRIP=<path-prefix> to write the encoded wires")
	}
	for _, name := range roundTripCorpora {
		canonical, err := DecodeResponsesRequest(loadRequestCorpus(t, name))
		if err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		wire, err := EncodeResponsesRequest(canonical)
		if err != nil {
			t.Fatalf("%s encode: %v", name, err)
		}
		path := prefix + "_" + name + ".json"
		if err := os.WriteFile(path, wire, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
	}
}

// roundTripCorpora are the captured conversations every leg of this file runs
// over. One list, so adding a capture cannot reach some tests and miss others.
var roundTripCorpora = []string{
	"openai_responses_tool_turn",
	"openai_responses_reasoning_tool_turn",
	"openai_responses_parallel_tool_turn",
}

func loadRequestCorpus(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(requestCorpusDir, name+".request.json")))
	if err != nil {
		t.Fatalf("read request corpus %q: %v\n"+
			"Re-capture with: python3 %s/capture.py %s", name, err, requestCorpusDir, name)
	}
	return raw
}

// TestDecodeResponsesRequest_ToolTurnReachesCanonical pins the decode leg.
//
// The assertion is not "a tool_calls field exists somewhere" but the specific
// consistency every chat upstream enforces: a tool-role message must answer a
// tool call declared by an EARLIER assistant message with the same id. Losing
// the assistant turn leaves the tool result orphaned, and both OpenAI and
// Anthropic answer 400 for that — the caller's conversation stops working with
// nothing pointing at the gateway.
func TestDecodeResponsesRequest_ToolTurnReachesCanonical(t *testing.T) {
	for _, name := range roundTripCorpora {
		t.Run(name, func(t *testing.T) {
			raw := loadRequestCorpus(t, name)
			canonical, err := DecodeResponsesRequest(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			// What the fixture actually contains decides what to demand of the
			// output — reading it from the corpus rather than restating it keeps
			// the two from drifting when the capture is refreshed.
			wantCalls := map[string]string{} // call_id → name
			gjson.GetBytes(raw, "input").ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").String() == "function_call" {
					wantCalls[item.Get("call_id").String()] = item.Get("name").String()
				}
				return true
			})
			if len(wantCalls) == 0 {
				t.Fatalf("the %s fixture carries no function_call item — it cannot test the "+
					"conversion it was captured for; re-capture it", name)
			}

			declared := map[string]string{}
			var orphans []string
			var nullContent int
			gjson.GetBytes(canonical, "messages").ForEach(func(_, m gjson.Result) bool {
				switch m.Get("role").String() {
				case "assistant":
					m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
						declared[tc.Get("id").String()] = tc.Get("function.name").String()
						return true
					})
				case "tool":
					id := m.Get("tool_call_id").String()
					if _, ok := declared[id]; !ok {
						orphans = append(orphans, id)
					}
				}
				// A turn whose content is JSON null is rejected outright by
				// OpenAI ("content must be a string or array"), so it is worth
				// naming separately from the orphan: it is the residue of an
				// input item the decoder walked past.
				if c := m.Get("content"); c.Exists() && c.Type == gjson.Null {
					nullContent++
				}
				return true
			})

			for id, name := range wantCalls {
				if declared[id] != name {
					t.Errorf("tool call %s should name %q, canonical says %q — the function_call item "+
						"carried the call and canonical lost it: %s", id, name, declared[id], canonical)
				}
			}
			if len(orphans) > 0 {
				t.Errorf("tool results %v answer calls no assistant message declares — this is the "+
					"exact shape OpenAI and Anthropic answer 400 for: %s", orphans, canonical)
			}
			if nullContent > 0 {
				t.Errorf("%d message(s) carry content:null — an input item was walked past and left "+
					"a turn behind that no upstream accepts: %s", nullContent, canonical)
			}
		})
	}
}

// TestEncodeResponsesRequest_ToolCallReachesTheWire pins the encode leg, which
// fails the same conversation in the other direction: a chat-completions caller
// auto-upgraded onto /v1/responses, or any ingress routed to a Responses target.
//
// The canonical body here is built by DECODING the captured request rather than
// written out, so the encode leg is measured against what the decode leg
// actually produces instead of against a second hand-written guess.
func TestEncodeResponsesRequest_ToolCallReachesTheWire(t *testing.T) {
	canonical, err := DecodeResponsesRequest(loadRequestCorpus(t, "openai_responses_tool_turn"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	wire, err := EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	calls := map[string]gjson.Result{}
	gjson.GetBytes(wire, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			calls[item.Get("call_id").String()] = item
		}
		return true
	})
	if len(calls) == 0 {
		t.Fatalf("the assistant's tool call did not reach the Responses wire — the function_call "+
			"item is absent, so the function_call_output that follows is orphaned: %s", wire)
	}
	for id, item := range calls {
		if got := item.Get("name").String(); got != "get_weather" {
			t.Errorf("function_call %s names %q, want get_weather: %s", id, got, wire)
		}
		if got := item.Get("arguments").String(); got != `{"city":"Paris"}` {
			t.Errorf("function_call %s arguments = %q, want the model's own JSON string: %s",
				id, got, wire)
		}
	}
}

// TestResponsesToolTurnSurvivesTheRoundTrip is the A→canonical→A leg for the
// conversation shape this codec had modelled in only one direction.
func TestResponsesToolTurnSurvivesTheRoundTrip(t *testing.T) {
	for _, name := range roundTripCorpora {
		t.Run(name, func(t *testing.T) {
			raw := loadRequestCorpus(t, name)
			canonical, err := DecodeResponsesRequest(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			wire, err := EncodeResponsesRequest(canonical)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			if before, after := toolTurnSignature(raw), toolTurnSignature(wire); before != after {
				t.Errorf("the tool turn did not survive Responses→canonical→Responses\n before:\n%s\n after:\n%s\n wire = %s",
					before, after, wire)
			}
		})
	}
}

// TestParallelToolCallsBecomeOneAssistantTurn pins the shape difference the two
// wires disagree on.
//
// /v1/responses lists two function_call items; chat-completions expects ONE
// assistant message carrying two tool_calls, followed by the two tool messages.
// Emitting two assistant messages instead describes a different conversation —
// the model called a tool, was interrupted, then called another — and OpenAI
// rejects the second tool message for answering a call the message before it
// did not make.
func TestParallelToolCallsBecomeOneAssistantTurn(t *testing.T) {
	raw := loadRequestCorpus(t, "openai_responses_parallel_tool_turn")

	var wireCalls int
	gjson.GetBytes(raw, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			wireCalls++
		}
		return true
	})
	if wireCalls < 2 {
		t.Fatalf("the fixture carries %d function_call items — it cannot test parallel calling; "+
			"re-capture it", wireCalls)
	}

	canonical, err := DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	var assistantTurns, callsInFirst int
	gjson.GetBytes(canonical, "messages").ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").String() != "assistant" {
			return true
		}
		assistantTurns++
		if assistantTurns == 1 {
			callsInFirst = int(m.Get("tool_calls.#").Int())
		}
		return true
	})

	if assistantTurns != 1 {
		t.Errorf("%d parallel calls produced %d assistant turns, want 1 — consecutive calls are one "+
			"turn on the chat wire: %s", wireCalls, assistantTurns, canonical)
	}
	if callsInFirst != wireCalls {
		t.Errorf("the assistant turn carries %d tool_calls, the wire had %d: %s",
			callsInFirst, wireCalls, canonical)
	}
}

// TestReasoningItemSurvivesTheRequestWaist holds the opaque half to the same
// bar. A reasoning item's encrypted_content is what lets the model continue the
// chain it started; the client cannot regenerate it and cannot read it. Dropping
// it returns a 200 with a model that has forgotten how it got here, which is
// the quietest way to lose a capability the caller paid for.
func TestReasoningItemSurvivesTheRequestWaist(t *testing.T) {
	raw := loadRequestCorpus(t, "openai_responses_reasoning_tool_turn")

	var blob string
	gjson.GetBytes(raw, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" {
			blob = item.Get("encrypted_content").String()
		}
		return blob == ""
	})
	if blob == "" {
		t.Fatal("the fixture carries no encrypted reasoning blob — re-capture it, or this test " +
			"proves nothing about the field it names")
	}

	canonical, err := DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	wire, err := EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var restored bool
	gjson.GetBytes(wire, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").String() == blob {
			restored = true
		}
		return !restored
	})
	if !restored {
		t.Errorf("the reasoning item's encrypted_content did not come back out — the model is "+
			"handed a conversation missing the chain it was continuing: %s", wire)
	}
}

// TestRoundTripWireMatchesWhatUpstreamAccepted turns a one-time observation
// into a standing gate.
//
// The `.roundtrip.request.json` files are the bytes this codec produced from
// the captured requests, POSTed back to OpenAI, answered 200, and answered
// ABOUT THE SAME CONVERSATION — the model reported the 18°C the tool result
// carried, which a request that had lost the tool turn could not have produced.
// Re-capture with:
//
//	python3 <corpus>/capture.py verify <name> <path-to-encoded-wire>
//
// Comparing against them here means a future edit to either leg has to answer
// for the difference against a shape a provider actually accepted, rather than
// against a shape that merely still parses. Values are compared, not bytes: the
// golden was written by the capture script's JSON serializer and this side by
// sjson, and neither one's key order is part of the contract.
func TestRoundTripWireMatchesWhatUpstreamAccepted(t *testing.T) {
	for _, name := range roundTripCorpora {
		t.Run(name, func(t *testing.T) {
			canonical, err := DecodeResponsesRequest(loadRequestCorpus(t, name))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			wire, err := EncodeResponsesRequest(canonical)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			goldenPath := filepath.Join(requestCorpusDir, name+".roundtrip.request.json")
			goldenRaw, err := os.ReadFile(filepath.Clean(goldenPath))
			if err != nil {
				t.Fatalf("read the upstream-accepted golden: %v", err)
			}

			var got, want any
			if err := json.Unmarshal(wire, &got); err != nil {
				t.Fatalf("the encoder produced invalid JSON: %v", err)
			}
			if err := json.Unmarshal(goldenRaw, &want); err != nil {
				t.Fatalf("parse golden: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				gotPretty, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("the encoded wire no longer matches the body OpenAI accepted\n got:\n%s\n want (%s):\n%s",
					gotPretty, goldenPath, goldenRaw)
			}
		})
	}
}

// toolTurnSignature names what a tool conversation is made of, in order: who
// spoke, what was called with which arguments, and which call each result
// answers. Comparing raw bytes would fail on key ordering and on the flat and
// nested tool spellings the wire accepts interchangeably; comparing only text
// would pass while the calls vanish.
func toolTurnSignature(wire []byte) string {
	var out []byte
	gjson.GetBytes(wire, "input").ForEach(func(_, item gjson.Result) bool {
		switch t := item.Get("type").String(); t {
		case "function_call":
			out = append(out, ("call:" + item.Get("call_id").String() + ":" +
				item.Get("name").String() + ":" + item.Get("arguments").String() + "\n")...)
		case "function_call_output":
			out = append(out, ("result:" + item.Get("call_id").String() + ":" +
				item.Get("output").String() + "\n")...)
		case "reasoning":
			out = append(out, ("reasoning:" + item.Get("encrypted_content").String() + "\n")...)
		default:
			role := item.Get("role").String()
			if role == "" {
				role = "?"
			}
			out = append(out, (role + ":")...)
			item.Get("content").ForEach(func(_, p gjson.Result) bool {
				out = append(out, (p.Get("type").String() + "=" + p.Get("text").String() + ";")...)
				return true
			})
			out = append(out, '\n')
		}
		return true
	})
	return string(out)
}
