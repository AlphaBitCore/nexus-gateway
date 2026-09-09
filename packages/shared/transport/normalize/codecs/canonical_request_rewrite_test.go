package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// A request that exercises every slot the decode produces a block for: a string
// content, an array content mixing text with a non-text part, an assistant turn
// replaying reasoning plus a tool call, and a tool result. The envelope fields
// around them are the ones a redaction must not disturb.
const canonicalRequestBody = `{
  "model": "gpt-4o",
  "stream": false,
  "temperature": 0.2,
  "tools": [{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
  "messages": [
    {"role":"system","content":"You are a careful assistant."},
    {"role":"user","name":"alice","content":[
      {"type":"text","text":"my ssn is 123-45-6789"},
      {"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
      {"type":"text","text":"and my card is 4111 1111 1111 1111"}
    ]},
    {"role":"assistant","reasoning_content":"the user gave a card number 4111 1111 1111 1111",
     "content":"Let me look that up.",
     "tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"ssn\":\"123-45-6789\"}"}}]},
    {"role":"tool","tool_call_id":"call_1","content":"record for 123-45-6789"}
  ]
}`

func decodeCanonicalRequest(t *testing.T, body string) core.NormalizedPayload {
	t.Helper()
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{
		AdapterType:  "openai",
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1/chat/completions",
		ContentType:  "application/json",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

// redactAll masks every text-bearing slot the way a real redactor would: the
// canonical payload is the ONLY thing it touches. Whether those edits reach the
// wire is exactly the property under test.
func redactAll(p core.NormalizedPayload) core.NormalizedPayload {
	for mi := range p.Messages {
		for bi := range p.Messages[mi].Content {
			b := &p.Messages[mi].Content[bi]
			switch b.Type {
			case core.ContentText, core.ContentReasoning:
				b.Text = strings.ReplaceAll(b.Text, "123-45-6789", "[REDACTED]")
				b.Text = strings.ReplaceAll(b.Text, "4111 1111 1111 1111", "[REDACTED]")
			case core.ContentToolResult:
				if b.ToolResult != nil {
					b.ToolResult.Output = strings.ReplaceAll(b.ToolResult.Output, "123-45-6789", "[REDACTED]")
				}
			case core.ContentToolUse:
				if b.ToolUse != nil {
					if v, ok := b.ToolUse.Input["ssn"].(string); ok && v == "123-45-6789" {
						b.ToolUse.Input["ssn"] = "[REDACTED]"
					}
				}
			}
		}
	}
	return p
}

// The load-bearing property: a redaction applied to the canonical payload
// reaches EVERY channel it was applied to. Before the request stage moved to the
// waist the write-back was positional and per-format, so channels the flat
// extractor never modelled — reasoning replayed on an assistant turn, a tool
// result — carried the unredacted text upstream while the audit row said
// redacted.
func TestRewriteCanonicalRequestContentLandsEveryRedaction(t *testing.T) {
	edited := redactAll(decodeCanonicalRequest(t, canonicalRequestBody))

	out, n, err := RewriteCanonicalRequestContent([]byte(canonicalRequestBody), edited)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n == 0 {
		t.Fatal("rewrite reported 0 writes for a body full of redactions")
	}
	// The raw bytes must not carry either secret anywhere — not in a channel the
	// walk skipped, not in a tool-call argument string.
	for _, secret := range []string{"123-45-6789", "4111 1111 1111 1111"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("redacted value %q survived into the rewritten request body:\n%s", secret, out)
		}
	}
	// And re-decoding must reproduce the edited payload, which is what proves the
	// redaction landed in the slot it was addressed to rather than merely
	// disappearing from the bytes.
	back := decodeCanonicalRequest(t, string(out))
	if len(back.Messages) != len(edited.Messages) {
		t.Fatalf("re-decode has %d messages, edited payload has %d", len(back.Messages), len(edited.Messages))
	}
	for mi := range edited.Messages {
		wantBlocks, gotBlocks := edited.Messages[mi].Content, back.Messages[mi].Content
		if len(wantBlocks) != len(gotBlocks) {
			t.Fatalf("message %d: re-decode has %d blocks, want %d", mi, len(gotBlocks), len(wantBlocks))
		}
		for bi := range wantBlocks {
			w, g := wantBlocks[bi], gotBlocks[bi]
			if w.Type != g.Type {
				t.Errorf("message %d block %d: type %s, want %s", mi, bi, g.Type, w.Type)
				continue
			}
			if w.Text != g.Text {
				t.Errorf("message %d block %d (%s): text %q, want %q", mi, bi, w.Type, g.Text, w.Text)
			}
			if w.ToolResult != nil && (g.ToolResult == nil || g.ToolResult.Output != w.ToolResult.Output) {
				t.Errorf("message %d block %d: tool result did not survive the rewrite", mi, bi)
			}
			if w.ToolUse != nil && g.ToolUse != nil {
				if g.ToolUse.Input["ssn"] != w.ToolUse.Input["ssn"] {
					t.Errorf("message %d block %d: tool-call argument %v, want %v",
						mi, bi, g.ToolUse.Input["ssn"], w.ToolUse.Input["ssn"])
				}
			}
		}
	}
}

// The envelope is not content and is never scanned, so a redaction must leave it
// exactly as the client sent it — including the fields that decide routing and
// billing.
func TestRewriteCanonicalRequestContentKeepsTheEnvelope(t *testing.T) {
	edited := redactAll(decodeCanonicalRequest(t, canonicalRequestBody))
	out, _, err := RewriteCanonicalRequestContent([]byte(canonicalRequestBody), edited)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for path, want := range map[string]string{
		"model":                                 "gpt-4o",
		"messages.0.role":                       "system",
		"messages.1.name":                       "alice",
		"messages.1.content.1.type":             "image_url",
		"messages.1.content.1.image_url.url":    "https://example.com/a.png",
		"messages.3.tool_call_id":               "call_1",
		"messages.2.tool_calls.0.id":            "call_1",
		"messages.2.tool_calls.0.function.name": "lookup",
		"tools.0.function.name":                 "lookup",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q after rewrite, want %q", path, got, want)
		}
	}
	if gjson.GetBytes(out, "temperature").Float() != 0.2 {
		t.Errorf("temperature = %v, want 0.2", gjson.GetBytes(out, "temperature").Value())
	}
	if gjson.GetBytes(out, "stream").Bool() {
		t.Error("stream flipped to true")
	}
}

// A request nothing redacted must come back byte-identical. Any rewrite that
// re-serializes the body would churn key order and whitespace on every request
// that merely passed a scan.
func TestRewriteCanonicalRequestContentIsIdentityWhenNothingChanged(t *testing.T) {
	p := decodeCanonicalRequest(t, canonicalRequestBody)
	out, n, err := RewriteCanonicalRequestContent([]byte(canonicalRequestBody), p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 0 {
		t.Errorf("reported %d writes for an unedited payload", n)
	}
	if string(out) != canonicalRequestBody {
		t.Errorf("body changed with no edit applied:\n%s", out)
	}
}

// Fail-closed, not best-effort. A payload that did not come from this body would
// write one turn's redaction onto another turn's text; a redaction on a part
// with no text slot would vanish and the original would go upstream. Both are
// silent leaks, so both must be errors.
func TestRewriteCanonicalRequestContentFailsClosed(t *testing.T) {
	t.Run("payload from a different body", func(t *testing.T) {
		other := decodeCanonicalRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if _, _, err := RewriteCanonicalRequestContent([]byte(canonicalRequestBody), other); err == nil {
			t.Fatal("rewrite accepted a payload decoded from another body")
		}
	})

	t.Run("edit on a part with no text slot", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
		p := decodeCanonicalRequest(t, body)
		// The decode gives an image part a media block with empty text; force the
		// shape a redactor would produce if it ever edited one.
		p.Messages[0].Content[0].Text = "[REDACTED]"
		if _, _, err := RewriteCanonicalRequestContent([]byte(body), p); err == nil {
			t.Fatal("rewrite silently dropped a redaction that had no wire slot")
		}
	})

	t.Run("payload with more blocks than the wire has slots", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		p := decodeCanonicalRequest(t, body)
		p.Messages[0].Content = append(p.Messages[0].Content,
			core.ContentBlock{Type: core.ContentText, Text: "extra"})
		if _, _, err := RewriteCanonicalRequestContent([]byte(body), p); err == nil {
			t.Fatal("rewrite accepted a payload carrying a block no wire slot could hold")
		}
	})
}

// The alias spelling is chosen by the wire, not by the rewriter's preference: a
// provider that sends `reasoning` must get `reasoning` back, or the redacted
// text lands in a field the upstream ignores while the original stays put.
func TestRewriteCanonicalRequestContentWritesReasoningOnTheWireSpelling(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","reasoning":"secret 123-45-6789","content":"ok"}]}`
	edited := redactAll(decodeCanonicalRequest(t, body))
	out, _, err := RewriteCanonicalRequestContent([]byte(body), edited)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning").String(); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("messages.0.reasoning = %q — the redaction did not land on the spelling the wire used", got)
	}
	if gjson.GetBytes(out, "messages.0.reasoning_content").Exists() {
		t.Error("rewrite invented a reasoning_content field the request never carried")
	}
}
