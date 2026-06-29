package openai

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestAdapter_MasksToolCallArgs(t *testing.T) {
	// The OpenAI canonical adapter is the one adapter that reconstructs masked
	// tool-call arguments onto its wire, so it implements traffic.ToolArgMasker
	// and reports true. This is what GuardToolArgMasking consults to allow the
	// rewrite (all other adapters fail closed).
	a := &Adapter{}
	if !a.MasksToolCallArgs() {
		t.Fatalf("openai adapter must report MasksToolCallArgs() == true")
	}
	var _ traffic.ToolArgMasker = a
	if !traffic.ToolArgMaskingSupported(a) {
		t.Fatalf("ToolArgMaskingSupported should be true for the openai adapter")
	}
	// A non-empty ToolCallArgs must NOT be guarded away for this adapter.
	if err := traffic.GuardToolArgMasking(a, traffic.NormalizedContent{ToolCallArgs: []string{`{"q":"x"}`}}); err != nil {
		t.Fatalf("guard should pass for masking adapter, got %v", err)
	}
}

func TestRewriteResponseBody_MasksToolCallArguments(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"sure","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"user@example.com\"}"}}]}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{
			Segments:     []string{"sure"},
			ToolCallArgs: []string{`{"q":"[REDACTED]"}`},
		})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if n != 2 { // 1 content + 1 tool call
		t.Fatalf("written=%d want 2", n)
	}
	// Content still rewritten correctly.
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "sure" {
		t.Fatalf("content=%q", got)
	}
	// Arguments masked, and still a valid JSON STRING holding valid JSON.
	argStr := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments")
	if argStr.Type != gjson.String {
		t.Fatalf("arguments is not a JSON string: %s", argStr.Type)
	}
	if !gjson.Valid(argStr.String()) {
		t.Fatalf("masked arguments not valid JSON: %q", argStr.String())
	}
	if gjson.Get(argStr.String(), "q").String() != "[REDACTED]" {
		t.Fatalf("arguments not masked: %q", argStr.String())
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("output body not valid JSON")
	}
}

func TestRewriteResponseBody_MultiChoiceMultiCallOrdering(t *testing.T) {
	// choice0: two function tool_calls; choice1: one. ToolCallArgs is in that
	// flattened document order — pin the index alignment (R3).
	body := []byte(`{"choices":[` +
		`{"message":{"content":"c0","tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"f0","arguments":"{\"x\":1}"}},` +
		`{"id":"b","type":"function","function":{"name":"f1","arguments":"{\"x\":2}"}}]}},` +
		`{"message":{"content":"c1","tool_calls":[` +
		`{"id":"c","type":"function","function":{"name":"f2","arguments":"{\"x\":3}"}}]}}` +
		`]}`)
	a := &Adapter{}
	out, _, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{
			Segments:     []string{"c0", "c1"},
			ToolCallArgs: []string{`{"x":"A"}`, `{"x":"B"}`, `{"x":"C"}`},
		})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	checks := map[string]string{
		"choices.0.message.tool_calls.0.function.arguments": `{"x":"A"}`,
		"choices.0.message.tool_calls.1.function.arguments": `{"x":"B"}`,
		"choices.1.message.tool_calls.0.function.arguments": `{"x":"C"}`,
	}
	for path, want := range checks {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q want %q", path, got, want)
		}
	}
}

func TestRewriteResponseBody_SkipsNonFunctionToolCall(t *testing.T) {
	// A non-function tool call sits between two function calls; it must be
	// skipped WITHOUT consuming an args slot, mirroring the canonical codec.
	body := []byte(`{"choices":[{"message":{"tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"f0","arguments":"{}"}},` +
		`{"id":"r","type":"retrieval"},` +
		`{"id":"b","type":"function","function":{"name":"f1","arguments":"{}"}}]}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{ToolCallArgs: []string{`{"masked":0}`, `{"masked":1}`}})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if n != 2 {
		t.Fatalf("written=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"masked":0}` {
		t.Fatalf("call0 args=%q", got)
	}
	// The retrieval call has no function.arguments and must be untouched.
	if gjson.GetBytes(out, "choices.0.message.tool_calls.1.type").String() != "retrieval" {
		t.Fatalf("non-function call mutated")
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.2.function.arguments").String(); got != `{"masked":1}` {
		t.Fatalf("call2 args=%q", got)
	}
}

func TestRewriteResponseBody_ToolOnlyResponseNoText(t *testing.T) {
	// Tool-only response: message.content is null. Empty Segments must NOT
	// starve the tool-args pass.
	body := []byte(`{"choices":[{"message":{"content":null,"tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"f","arguments":"{\"ssn\":\"111-22-3333\"}"}}]}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{ToolCallArgs: []string{`{"ssn":"[REDACTED]"}`}})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if n != 1 {
		t.Fatalf("written=%d want 1", n)
	}
	if got := gjson.Get(gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(), "ssn").String(); got != "[REDACTED]" {
		t.Fatalf("tool-only args not masked: %q", got)
	}
}

func TestRewriteResponseBody_EmptySentinelLeavesSiblingByteIdentical(t *testing.T) {
	// Two function tool calls; only call[0] was masked. call[1] carries the
	// empty-string sentinel — its wire `arguments` must remain BYTE-FOR-BYTE
	// identical (no re-marshal, no key reorder, no float-precision loss).
	// call[1] uses an argument shape (unsorted keys + a high-precision float)
	// that a re-marshal would visibly perturb, so byte-identity is a real
	// assertion, not a coincidence.
	sibling := `{\"zeta\":1,\"alpha\":2,\"ratio\":0.30000000000000004}`
	body := []byte(`{"choices":[{"message":{"tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"f0","arguments":"{\"email\":\"user@example.com\"}"}},` +
		`{"id":"b","type":"function","function":{"name":"f1","arguments":"` + sibling + `"}}]}}]}`)
	before := gjson.GetBytes(body, "choices.0.message.tool_calls.1.function.arguments").String()
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{ToolCallArgs: []string{`{"email":"[REDACTED]"}`, ""}})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if n != 1 { // only call[0] written; the "" sentinel consumes its slot but is not counted
		t.Fatalf("written=%d want 1 (only the masked call)", n)
	}
	// call[0] masked.
	if got := gjson.Get(gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(), "email").String(); got != "[REDACTED]" {
		t.Fatalf("call0 not masked: %q", got)
	}
	// call[1] byte-for-byte identical to the input wire.
	after := gjson.GetBytes(out, "choices.0.message.tool_calls.1.function.arguments").String()
	if after != before {
		t.Fatalf("sibling call clobbered:\n before=%q\n after =%q", before, after)
	}
}

func TestRewriteResponsesResponseBody_EmptySentinelLeavesSiblingByteIdentical(t *testing.T) {
	// Responses-API variant: two function_call items, only the first masked.
	sibling := `{\"zeta\":1,\"alpha\":2,\"ratio\":0.30000000000000004}`
	body := []byte(`{"output":[` +
		`{"type":"function_call","name":"f0","arguments":"{\"email\":\"user@example.com\"}"},` +
		`{"type":"function_call","name":"f1","arguments":"` + sibling + `"}]}`)
	before := gjson.GetBytes(body, "output.1.arguments").String()
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{ToolCallArgs: []string{`{"email":"[REDACTED]"}`, ""}})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if n != 1 {
		t.Fatalf("written=%d want 1", n)
	}
	if after := gjson.GetBytes(out, "output.1.arguments").String(); after != before {
		t.Fatalf("sibling function_call clobbered:\n before=%q\n after =%q", before, after)
	}
}

func TestRewriteResponseBody_NilToolCallArgsZeroChurn(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi","tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"f","arguments":"{\"q\":\"secret\"}"}}]}}]}`)
	a := &Adapter{}
	out, _, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"hi"}}) // ToolCallArgs nil
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"q":"secret"}` {
		t.Fatalf("nil ToolCallArgs should leave args untouched, got %q", got)
	}
}

func TestRewriteRequestBody_MasksHistoryToolCallArguments(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"ok","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"send","arguments":"{\"to\":\"a@b.com\"}"}}]}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{
			Segments:     []string{"go", "ok"},
			ToolCallArgs: []string{`{"to":"[REDACTED]"}`},
		})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if n != 3 { // 2 text + 1 tool call
		t.Fatalf("written=%d want 3", n)
	}
	if got := gjson.Get(gjson.GetBytes(out, "messages.1.tool_calls.0.function.arguments").String(), "to").String(); got != "[REDACTED]" {
		t.Fatalf("history tool args not masked: %q", got)
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("output not valid JSON")
	}
}

func TestRewriteResponsesResponseBody_MasksFunctionCallArgs(t *testing.T) {
	body := []byte(`{"output":[` +
		`{"type":"message","content":[{"type":"output_text","text":"done"}]},` +
		`{"type":"function_call","name":"f","arguments":"{\"q\":\"pii\"}"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{
			Segments:     []string{"done"},
			ToolCallArgs: []string{`{"q":"[REDACTED]"}`},
		})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if n != 2 {
		t.Fatalf("written=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "output.0.content.0.text").String(); got != "done" {
		t.Fatalf("output_text=%q", got)
	}
	if got := gjson.Get(gjson.GetBytes(out, "output.1.arguments").String(), "q").String(); got != "[REDACTED]" {
		t.Fatalf("function_call args not masked: %q", got)
	}
}

func TestRewriteResponsesCreate_MasksFunctionCallArgs(t *testing.T) {
	body := []byte(`{"model":"gpt-4","input":[` +
		`{"role":"user","content":"hi"},` +
		`{"type":"function_call","name":"f","arguments":"{\"q\":\"pii\"}"}]}`)
	a := &Adapter{}
	out, _, err := a.RewriteRequestBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{
			Segments:     []string{"hi"},
			ToolCallArgs: []string{`{"q":"[REDACTED]"}`},
		})
	if err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if got := gjson.Get(gjson.GetBytes(out, "input.1.arguments").String(), "q").String(); got != "[REDACTED]" {
		t.Fatalf("responses-create function_call args not masked: %q", got)
	}
}
