package ingress

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Gemini uses ONE part shape for every attachment, so the declared mime type is
// the only thing that says which modality it is. Reading them all as images
// makes a PDF arrive at the next wire claiming to be one — a masquerade the
// canonical's separate file part exists to prevent, and one the receiving model
// answers by describing an image it cannot see.
func TestGenerateContentRequest_ANonImageAttachmentIsNotForwardedAsAnImage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		part     string
		wantType string
		wantAt   string // where the payload must land
	}{
		{
			"an inline PDF is a file",
			`{"inlineData":{"mimeType":"application/pdf","data":"JVBER"}}`,
			"file", "messages.0.content.1.file.file_data",
		},
		{
			"an inline PNG is an image",
			`{"inlineData":{"mimeType":"image/png","data":"iVBOR"}}`,
			"image_url", "messages.0.content.1.image_url.url",
		},
		{
			"a hosted PDF is a file",
			`{"fileData":{"mimeType":"application/pdf","fileUri":"https://x/y.pdf"}}`,
			"file", "messages.0.content.1.file.file_url",
		},
		{
			"a hosted PNG is an image",
			`{"fileData":{"mimeType":"image/png","fileUri":"https://x/y.png"}}`,
			"image_url", "messages.0.content.1.image_url.url",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(
				`{"contents":[{"role":"user","parts":[{"text":"look"},`+tc.part+`]}]}`), "m")
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if got := gjson.GetBytes(out, "messages.0.content.1.type").String(); got != tc.wantType {
				t.Fatalf("part type = %q, want %q\n%s", got, tc.wantType, out)
			}
			if !gjson.GetBytes(out, tc.wantAt).Exists() {
				t.Fatalf("payload missing at %s\n%s", tc.wantAt, out)
			}
		})
	}
}

// A thought part is the model's reasoning, not its answer. Folding it into the
// visible text would corrupt a replayed history — the next turn would read the
// model's private deliberation back to it as something it had said out loud.
func TestGenerateContentRequest_AThoughtPartIsReasoningNotAnswer(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(
		`{"contents":[{"role":"model","parts":[`+
			`{"text":"first I weigh it","thought":true},`+
			`{"text":"and the answer is 4"}]}]}`), "m")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "and the answer is 4" {
		t.Fatalf("visible content = %q, want only the answer\n%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "first I weigh it" {
		t.Fatalf("reasoning_content = %q, want the thought\n%s", got, out)
	}
}

// Parallel calls to the same function are paired with their responses in order.
// Gemini pairs them by NAME when the model emits no id — every model before
// Gemini 3 — so two calls to one function must not both correlate to the first
// id, which would tell the upstream that one result answered a call twice and
// the other answered nothing.
func TestGenerateContentRequest_ParallelCallsToOneFunctionPairInOrder(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(
		`{"contents":[`+
			`{"role":"model","parts":[`+
			`{"functionCall":{"name":"lookup","args":{"q":"a"}}},`+
			`{"functionCall":{"name":"lookup","args":{"q":"b"}}}]},`+
			`{"role":"user","parts":[`+
			`{"functionResponse":{"name":"lookup","response":{"r":1}}},`+
			`{"functionResponse":{"name":"lookup","response":{"r":2}}}]}]}`), "m")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	callA := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	callB := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String()
	if callA == "" || callB == "" || callA == callB {
		t.Fatalf("two distinct call ids expected, got %q and %q\n%s", callA, callB, out)
	}
	respA := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	respB := gjson.GetBytes(out, "messages.2.tool_call_id").String()
	if respA != callA || respB != callB {
		t.Fatalf("responses paired to (%q,%q), want (%q,%q)\n%s", respA, respB, callA, callB, out)
	}
}

func TestGenerateContentRequest_ParallelIdenticalCallsKeepSignatureAndFIFO(t *testing.T) {
	body := []byte(`{"contents":[` +
		`{"role":"model","parts":[` +
		`{"functionCall":{"name":"lookup","args":{"q":"same"}},"thoughtSignature":"sig-a"},` +
		`{"functionCall":{"name":"lookup","args":{"q":"same"}},"thoughtSignature":"sig-b"}]},` +
		`{"role":"user","parts":[` +
		`{"functionResponse":{"name":"lookup","response":{"r":1}}},` +
		`{"functionResponse":{"name":"lookup","response":{"r":2}}}]}]}`)
	out, err := GenerateContentRequestToOpenAIChatCompletion(body, "m")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	callA := gjson.GetBytes(out, "messages.0.tool_calls.0")
	callB := gjson.GetBytes(out, "messages.0.tool_calls.1")
	if callA.Get("id").String() == "" || callA.Get("id").String() == callB.Get("id").String() {
		t.Fatalf("identical calls need stable distinct IDs: %s", out)
	}
	if callA.Get("function.thought_signature").String() != "sig-a" || callB.Get("function.thought_signature").String() != "sig-b" {
		t.Fatalf("signatures detached from their Parts: %s", out)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != callA.Get("id").String() {
		t.Fatalf("first response id=%q, want %q", got, callA.Get("id").String())
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != callB.Get("id").String() {
		t.Fatalf("second response id=%q, want %q", got, callB.Get("id").String())
	}
	out2, err := GenerateContentRequestToOpenAIChatCompletion(body, "m")
	if err != nil {
		t.Fatalf("canonicalize replay: %v", err)
	}
	if got, want := gjson.GetBytes(out2, "messages.0.tool_calls.0.id").String(), callA.Get("id").String(); got != want {
		t.Fatalf("replay changed first id: %q vs %q", got, want)
	}
}
