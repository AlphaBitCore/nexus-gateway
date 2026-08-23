package canonicalbridge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// The content axis of the round-trip standard (provider-adapter-architecture.md
// §3). TestShapeRoundTripIdentity beside this file proves the same A→B→A′ chain,
// but over a MINIMAL fixture — one user turn of plain text — compared on an
// (role, text) signature. That shape cannot express the defects the conversions
// actually produce: a dropped image, a tool call whose arguments are rewritten,
// a tool result that no longer names the call it answers, or a media block
// silently replaced by a text placeholder. Each of those passes a text-only
// signature untouched.
//
// So this file carries a fixture with everything a real conversation has —
// system instruction, text plus an image plus a PDF in one turn, a declared
// tool, an assistant tool call, its result, and a following turn — and a signature that
// names every one of those parts. What the fixture does not carry, the gate
// cannot defend, which is why the fixture is the load-bearing half.
func richNativeChatBody(shape provcore.Format) string {
	switch shape {
	case provcore.FormatOpenAI:
		return `{"model":"gpt-4o-mini","max_tokens":32,
"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
"messages":[
 {"role":"system","content":"be terse"},
 {"role":"user","content":[{"type":"text","text":"weather?"},{"type":"image_url","image_url":{"url":"https://ex.com/a.png"}},{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0x"}}]},
 {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
 {"role":"tool","tool_call_id":"c1","content":"18C"},
 {"role":"user","content":"thanks"}]}`
	case provcore.FormatAnthropic:
		return `{"model":"claude-3-5-haiku-20240307","max_tokens":32,
"system":"be terse",
"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
"messages":[
 {"role":"user","content":[{"type":"text","text":"weather?"},{"type":"image","source":{"type":"url","url":"https://ex.com/a.png"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"get_weather","input":{"city":"SF"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"18C"}]},
 {"role":"user","content":[{"type":"text","text":"thanks"}]}]}`
	case provcore.FormatGemini:
		return `{"systemInstruction":{"parts":[{"text":"be terse"}]},
"tools":[{"functionDeclarations":[{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}],
"contents":[
 {"role":"user","parts":[{"text":"weather?"},{"fileData":{"mimeType":"image/png","fileUri":"https://ex.com/a.png"}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0x"}}]},
 {"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"SF"}}}]},
 {"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"result":"18C"}}}]},
 {"role":"user","parts":[{"text":"thanks"}]}],
"generationConfig":{"maxOutputTokens":32}}`
	}
	return ""
}

func TestShapeRoundTripIdentity_RichContent(t *testing.T) {
	b := testBridge(t)
	shapes := []provcore.Format{provcore.FormatOpenAI, provcore.FormatAnthropic, provcore.FormatGemini}

	for _, a := range shapes {
		for _, viaB := range shapes {
			if a == viaB {
				continue
			}
			t.Run(string(a)+"_via_"+string(viaB)+"_back", func(t *testing.T) {
				if !b.ChatRoutable(a, viaB) || !b.ChatRoutable(viaB, a) {
					t.Skipf("%s ↔ %s not routable", a, viaB)
				}
				bodyA := []byte(richNativeChatBody(a))

				wireB, _, err := b.IngressChatToWire(a, viaB, bodyA, dummyCallTarget(viaB), false)
				if err != nil {
					t.Fatalf("A→B (%s→%s): %v", a, viaB, err)
				}
				bodyA2, _, err := b.IngressChatToWire(viaB, a, wireB, dummyCallTarget(a), false)
				if err != nil {
					t.Fatalf("B→A (%s→%s): %v", viaB, a, err)
				}

				canonA, err := b.IngressChatToCanonical(a, bodyA, dummyCallTarget(a))
				if err != nil {
					t.Fatalf("canonicalize original A: %v", err)
				}
				canonA2, err := b.IngressChatToCanonical(a, bodyA2, dummyCallTarget(a))
				if err != nil {
					t.Fatalf("canonicalize round-tripped A: %v", err)
				}
				sig, sig2 := richChatSignature(canonA), richChatSignature(canonA2)
				if sig != sig2 {
					t.Errorf("round-trip A→%s→A lost content\n original :\n%s\n roundtrip:\n%s\n wireB = %s",
						viaB, sig, sig2, wireB)
				}
			})
		}
	}
}

// Every canonical tool message must quote the id of a tool call that actually
// appears in the same conversation. An OpenAI-compatible upstream correlates
// them by that id; a tool result naming a call it was never given is a 400 at
// best and a silently dropped result at worst.
//
// This is separate from the signature comparison above because it is not a
// round-trip property: a body can be self-consistent before AND after the trip
// while being internally broken at both ends. Gemini pairs the two by function
// NAME on the wire and emits no id before Gemini 3, so the canonical id has to
// be synthesized — and the call side and the response side each synthesized
// their own, which never matched.
func TestCanonicalToolResultsReferenceARealCall(t *testing.T) {
	b := testBridge(t)
	for _, shape := range []provcore.Format{provcore.FormatOpenAI, provcore.FormatAnthropic, provcore.FormatGemini} {
		t.Run(string(shape), func(t *testing.T) {
			canonical, err := b.IngressChatToCanonical(shape, []byte(richNativeChatBody(shape)), dummyCallTarget(shape))
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			callIDs := map[string]bool{}
			gjson.GetBytes(canonical, "messages").ForEach(func(_, m gjson.Result) bool {
				m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
					callIDs[tc.Get("id").String()] = true
					return true
				})
				return true
			})
			if len(callIDs) == 0 {
				t.Fatal("fixture produced no tool calls — the check would pass vacuously")
			}
			gjson.GetBytes(canonical, "messages").ForEach(func(_, m gjson.Result) bool {
				if m.Get("role").String() != "tool" {
					return true
				}
				id := m.Get("tool_call_id").String()
				if !callIDs[id] {
					t.Errorf("tool message quotes tool_call_id %q, which no tool call in this conversation declares (declared: %v)",
						id, sortedKeys(callIDs))
				}
				return true
			})
		})
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// richChatSignature names every content-bearing part of a canonical chat body:
// role, text, each media reference, each tool call's name and arguments, and
// each tool result keyed by the call it answers.
//
// Deliberately excluded, because these differ per hop by design and are not
// fidelity losses:
//   - model + protocol-default backfill (max_tokens, image detail:"auto")
//   - JSON object key ORDER inside tool schemas and tool-call arguments, which
//     each hop re-serializes; canonJSON normalizes it so a reordered schema is
//     not reported as a lost one
//   - the literal tool_call_id STRING, since a wire that carries no id forces
//     one to be synthesized; the linkage between call and result is asserted by
//     TestCanonicalToolResultsReferenceARealCall instead, which is the property
//     that actually matters
//   - the exact tool-result payload when the trip passes through Gemini, whose
//     functionResponse.response is documented as an object: a bare "18C" must be
//     wrapped as {"result":"18C"} to satisfy the schema, and unwrapping it on
//     return would corrupt a genuinely object-shaped result from a Gemini-native
//     tool. Normalized below rather than ignored, so a result that changes in
//     any OTHER way still fails.
func richChatSignature(canonical []byte) string {
	var sig strings.Builder
	gjson.GetBytes(canonical, "tools").ForEach(func(_, tool gjson.Result) bool {
		fn := tool.Get("function")
		sig.WriteString("tool-def " + fn.Get("name").String() + " " + canonJSON(fn.Get("parameters").Raw) + "\n")
		return true
	})
	gjson.GetBytes(canonical, "messages").ForEach(func(_, m gjson.Result) bool {
		role := m.Get("role").String()
		sig.WriteString(role + ":")
		if c := m.Get("content"); c.IsArray() {
			c.ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "text":
					sig.WriteString(" text=" + part.Get("text").String())
				case "image_url":
					sig.WriteString(" image=" + part.Get("image_url.url").String())
				case "input_audio":
					sig.WriteString(" audio=" + part.Get("input_audio.data").String())
				case "file":
					// Keyed on the bytes / locator, not the whole object: a
					// filename is carried by only some wires, so requiring it
					// would report a fidelity loss where none happened.
					sig.WriteString(" file=" + firstNonEmpty(
						part.Get("file.file_data").String(),
						part.Get("file.file_url").String(),
						part.Get("file.file_id").String()))
				default:
					sig.WriteString(" " + part.Get("type").String() + "=" + part.Raw)
				}
				return true
			})
		} else if role == "tool" {
			sig.WriteString(" result=" + unwrapGeminiResultEnvelope(c.String()))
		} else if c.Exists() && c.Type != gjson.Null {
			sig.WriteString(" text=" + c.String())
		}
		m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			fn := tc.Get("function")
			fmt.Fprintf(&sig, " call=%s(%s)", fn.Get("name").String(), canonJSON(fn.Get("arguments").String()))
			return true
		})
		sig.WriteString("\n")
		return true
	})
	return sig.String()
}

// canonJSON re-serializes a JSON document with map keys in sorted order so two
// documents that differ only in field order compare equal. Non-JSON input is
// returned unchanged — a tool whose arguments are not valid JSON is still
// compared verbatim rather than silently normalized to nothing.
func canonJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
}

// unwrapGeminiResultEnvelope strips the {"result": …} object the Gemini codec
// must wrap a non-object tool result in. Only the exact single-key shape is
// unwrapped, so a tool that genuinely returns {"result": …} plus anything else
// is left alone and still compared in full.
func unwrapGeminiResultEnvelope(s string) string {
	v := gjson.Parse(s)
	if !v.IsObject() {
		return s
	}
	keys := v.Map()
	if len(keys) != 1 {
		return s
	}
	inner, ok := keys["result"]
	if !ok {
		return s
	}
	if inner.Type == gjson.String {
		return inner.String()
	}
	return inner.Raw
}
