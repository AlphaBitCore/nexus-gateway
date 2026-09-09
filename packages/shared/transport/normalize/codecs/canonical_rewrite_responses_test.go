package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// A /v1/responses body carrying every channel the decode produces a block for,
// in the order it produces them: a reasoning summary, a message with two text
// parts, and a function call. The fields around them are the ones a redaction
// must not disturb — and the ones a decode-and-re-encode round trip loses.
const responsesRewriteBody = `{"id":"resp_1","object":"response","model":"gpt-5",` +
	`"status":"completed","previous_response_id":"resp_0","service_tier":"default",` +
	`"instructions":"be brief","temperature":0.3,` +
	`"output":[` +
	`{"id":"rs_1","type":"reasoning","encrypted_content":"OPAQUE","summary":[` +
	`{"type":"summary_text","text":"the ssn 123-45-6789 was given"}]},` +
	`{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[` +
	`{"type":"output_text","text":"I will not repeat 123-45-6789.","annotations":[{"type":"url_citation","url":"https://example.com"}]},` +
	`{"type":"output_text","text":"Anything else?","annotations":[]}]},` +
	`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup",` +
	`"arguments":"{\"ssn\":\"123-45-6789\"}"}` +
	`],"usage":{"input_tokens":4,"output_tokens":7,"total_tokens":11}}`

func decodeResponsesBody(t *testing.T, body string) core.NormalizedPayload {
	t.Helper()
	p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), []byte(body), core.Meta{
		AdapterType:  "openai",
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1/responses",
		ContentType:  "application/json",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func maskSecretEverywhere(p core.NormalizedPayload, secret string) core.NormalizedPayload {
	for mi := range p.Messages {
		for bi := range p.Messages[mi].Content {
			b := &p.Messages[mi].Content[bi]
			b.Text = strings.ReplaceAll(b.Text, secret, "[REDACTED]")
			if b.ToolUse != nil {
				if v, ok := b.ToolUse.Input["ssn"].(string); ok && v == secret {
					b.ToolUse.Input["ssn"] = "[REDACTED]"
				}
			}
		}
	}
	return p
}

// The load-bearing property, and the reason this rewriter exists instead of a
// decode-to-chat-and-re-encode: every redaction lands, and everything else is
// still there afterwards. The round trip masked the text correctly too — it just
// rebuilt the envelope around it.
func TestRewriteCanonicalResponsesContentEditsInPlace(t *testing.T) {
	const secret = "123-45-6789"
	edited := maskSecretEverywhere(decodeResponsesBody(t, responsesRewriteBody), secret)

	out, n, err := RewriteCanonicalResponsesContent([]byte(responsesRewriteBody), edited)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n == 0 {
		t.Fatal("no writes for a body full of redactions")
	}
	if strings.Contains(string(out), secret) {
		t.Errorf("the secret survived somewhere in the rewritten body:\n%s", out)
	}
	// Every field a re-encode dropped. This list is the actual diff that made
	// the round trip unacceptable, so it is what the test asserts.
	for path, want := range map[string]string{
		"previous_response_id":                 "resp_0", // conversation chaining
		"output.0.id":                          "rs_1",   // ids must not be regenerated
		"output.0.encrypted_content":           "OPAQUE", // needed to continue with store:false
		"output.1.id":                          "msg_1",
		"output.1.content.0.annotations.0.url": "https://example.com", // a web-search answer must display its citation
		"output.2.id":                          "fc_1",
		"output.2.call_id":                     "call_1",
		"instructions":                         "be brief",
		"service_tier":                         "default",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q — the redaction rebuilt the body instead of editing it",
				path, got, want)
		}
	}
	// And the order of output[] is preserved: a re-encode moved tool calls to
	// the end, which breaks any client reading items positionally.
	types := []string{}
	gjson.GetBytes(out, "output").ForEach(func(_, item gjson.Result) bool {
		types = append(types, item.Get("type").String())
		return true
	})
	if strings.Join(types, ",") != "reasoning,message,function_call" {
		t.Errorf("output[] order = %v, want the input order", types)
	}

	// Re-decoding must reproduce the edited payload, which is what proves each
	// redaction landed in the slot it was addressed to.
	back := decodeResponsesBody(t, string(out))
	if len(back.Messages) != 1 || len(back.Messages[0].Content) != len(edited.Messages[0].Content) {
		t.Fatalf("re-decode has a different block count: %d", len(back.Messages[0].Content))
	}
	for i, w := range edited.Messages[0].Content {
		g := back.Messages[0].Content[i]
		if w.Type != g.Type {
			t.Errorf("block %d: type %s, want %s", i, g.Type, w.Type)
		}
		if w.Text != g.Text {
			t.Errorf("block %d (%s): text %q, want %q", i, w.Type, g.Text, w.Text)
		}
		if w.ToolUse != nil && g.ToolUse != nil && g.ToolUse.Input["ssn"] != w.ToolUse.Input["ssn"] {
			t.Errorf("block %d: tool argument %v, want %v", i, g.ToolUse.Input["ssn"], w.ToolUse.Input["ssn"])
		}
	}
}

func TestRewriteCanonicalResponsesContentIsIdentityWhenNothingChanged(t *testing.T) {
	p := decodeResponsesBody(t, responsesRewriteBody)
	out, n, err := RewriteCanonicalResponsesContent([]byte(responsesRewriteBody), p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 0 {
		t.Errorf("reported %d writes for an unedited payload", n)
	}
	if string(out) != responsesRewriteBody {
		t.Errorf("body changed with no edit applied:\n%s", out)
	}
}

// A tool call whose arguments arrive PARSED rather than as a JSON string: the
// decode reads whichever is present, so the rewrite has to write back to the
// same one or the edit lands in a field nothing reads.
func TestRewriteCanonicalResponsesContentWritesParsedToolInput(t *testing.T) {
	const body = `{"id":"r","object":"response","model":"m","status":"completed","output":[` +
		`{"id":"fc_1","type":"function_call","call_id":"c1","name":"lookup",` +
		`"input":{"ssn":"123-45-6789"}}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	edited := maskSecretEverywhere(decodeResponsesBody(t, body), "123-45-6789")

	out, n, err := RewriteCanonicalResponsesContent([]byte(body), edited)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n == 0 {
		t.Fatal("no write for a masked tool argument")
	}
	if got := gjson.GetBytes(out, "output.0.input.ssn").String(); got != "[REDACTED]" {
		t.Errorf("output.0.input.ssn = %q — the edit did not reach the parsed input", got)
	}
	if gjson.GetBytes(out, "output.0.arguments").Exists() {
		t.Error("the rewrite invented an arguments string the wire never carried")
	}
}

// Fail-closed on a payload that did not come from this body: writing a prefix
// would put one item's redaction onto another item's text.
func TestRewriteCanonicalResponsesContentFailsClosed(t *testing.T) {
	t.Run("payload from a different body", func(t *testing.T) {
		other := decodeResponsesBody(t, `{"id":"r","object":"response","model":"m",`+
			`"status":"completed","output":[{"type":"message","role":"assistant","content":[`+
			`{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		if _, _, err := RewriteCanonicalResponsesContent([]byte(responsesRewriteBody), other); err == nil {
			t.Fatal("accepted a payload decoded from another body")
		}
	})

	t.Run("no output[]", func(t *testing.T) {
		p := decodeResponsesBody(t, responsesRewriteBody)
		if _, _, err := RewriteCanonicalResponsesContent([]byte(`{"id":"r"}`), p); err == nil {
			t.Fatal("accepted a body with no output[]")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		p := decodeResponsesBody(t, responsesRewriteBody)
		if _, _, err := RewriteCanonicalResponsesContent([]byte(`{`), p); err == nil {
			t.Fatal("accepted a body that is not JSON")
		}
	})

	t.Run("more blocks than the wire has slots", func(t *testing.T) {
		p := decodeResponsesBody(t, responsesRewriteBody)
		p.Messages[0].Content = append(p.Messages[0].Content,
			core.ContentBlock{Type: core.ContentText, Text: "extra"})
		if _, _, err := RewriteCanonicalResponsesContent([]byte(responsesRewriteBody), p); err == nil {
			t.Fatal("accepted a payload carrying a block no slot could hold")
		}
	})
}

// The presence check has to know this shape. Asking the CHAT check about an
// output[] body answers "no content", and the fail-closed guard that consumes it
// would then read the most dangerous case as the safest one.
func TestCanonicalResponsesHasContent(t *testing.T) {
	if !CanonicalResponsesHasContent([]byte(responsesRewriteBody)) {
		t.Error("a body with reasoning, text and a tool call reported no content")
	}
	if CanonicalResponseHasContent([]byte(responsesRewriteBody)) {
		t.Error("the CHAT check claims to understand an output[] body; if it did, the shape " +
			"dispatch in front of it would be pointless and this test would be asserting nothing")
	}
	for name, body := range map[string]string{
		"no output":         `{"id":"r"}`,
		"empty output":      `{"id":"r","output":[]}`,
		"output not array":  `{"id":"r","output":"x"}`,
		"reasoning no text": `{"id":"r","output":[{"type":"reasoning","summary":[{"text":""}]}]}`,
		"message no text":   `{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":""}]}]}`,
	} {
		if CanonicalResponsesHasContent([]byte(body)) {
			t.Errorf("%s: reported content where there is none", name)
		}
	}
	// A tool-only response IS something to scan — the arguments are
	// model-authored text.
	if !CanonicalResponsesHasContent([]byte(`{"id":"r","output":[{"type":"function_call","name":"f"}]}`)) {
		t.Error("a tool-only response reported nothing to scan")
	}
}
