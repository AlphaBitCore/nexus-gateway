package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// everyContentChannel carries each scannable channel with a DISTINCT marker, so
// a redaction landing on the wrong channel is visible rather than plausible.
// The envelope fields are here to be left alone.
const everyContentChannel = `{
  "id": "chatcmpl-rw",
  "object": "chat.completion",
  "created": 1735689600,
  "model": "deepseek-reasoner",
  "system_fingerprint": "fp_keepme",
  "service_tier": "scale",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "reasoning_content": "REASONING-ORIGINAL",
      "content": "CONTENT-ORIGINAL",
      "refusal": "REFUSAL-ORIGINAL",
      "tool_calls": [{
        "id": "call_1",
        "type": "function",
        "function": {"name": "lookup", "arguments": "{\"q\":\"ARGS-ORIGINAL\"}"}
      }]
    },
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 11, "completion_tokens": 22, "total_tokens": 33}
}`

func decodeEveryChannel(t *testing.T) core.NormalizedPayload {
	t.Helper()
	p, err := SharedOpenAIChat().Normalize(context.Background(), []byte(everyContentChannel), core.Meta{
		AdapterType:  "openai",
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

// TestRedactionLandsOnTheChannelItCameFrom is the gate on the misalignment
// class. Each channel is redacted one at a time and every OTHER channel must be
// untouched — an off-by-one in the walk shows up as the neighbour changing.
func TestRedactionLandsOnTheChannelItCameFrom(t *testing.T) {
	channels := []struct {
		name      string
		blockType core.ContentType
		wirePath  string
	}{
		{"reasoning", core.ContentReasoning, "choices.0.message.reasoning_content"},
		{"content", core.ContentText, "choices.0.message.content"},
		{"refusal", core.ContentRefusal, "choices.0.message.refusal"},
	}

	for _, ch := range channels {
		t.Run(ch.name, func(t *testing.T) {
			p := decodeEveryChannel(t)
			edited := false
			for mi := range p.Messages {
				for bi := range p.Messages[mi].Content {
					if p.Messages[mi].Content[bi].Type == ch.blockType && !edited {
						p.Messages[mi].Content[bi].Text = "[REDACTED-" + ch.name + "]"
						edited = true
					}
				}
			}
			if !edited {
				t.Fatalf("no %s block decoded from a body that carries one", ch.name)
			}

			out, n, err := RewriteCanonicalResponseContent([]byte(everyContentChannel), p)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if n != 1 {
				t.Errorf("rewrote %d slots for a single-channel redaction, want 1", n)
			}

			want := "[REDACTED-" + ch.name + "]"
			if got := gjson.GetBytes(out, ch.wirePath).Str; got != want {
				t.Errorf("%s = %q, want %q — the redaction did not land on the channel it came from",
					ch.wirePath, got, want)
			}
			for _, other := range channels {
				if other.name == ch.name {
					continue
				}
				got := gjson.GetBytes(out, other.wirePath).Str
				if !strings.HasSuffix(got, "-ORIGINAL") {
					t.Errorf("redacting %s also changed %s to %q — the walk is off by one, which "+
						"is how a redacted chain-of-thought ends up delivered as the answer",
						ch.name, other.wirePath, got)
				}
			}
		})
	}
}

// TestRewriteLeavesTheEnvelopeAlone pins the reason a redaction cannot lose a
// field: the rewrite never writes outside the content channels.
func TestRewriteLeavesTheEnvelopeAlone(t *testing.T) {
	p := decodeEveryChannel(t)
	for mi := range p.Messages {
		for bi := range p.Messages[mi].Content {
			if p.Messages[mi].Content[bi].Type == core.ContentText {
				p.Messages[mi].Content[bi].Text = "[REDACTED]"
			}
		}
	}
	out, _, err := RewriteCanonicalResponseContent([]byte(everyContentChannel), p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	envelope := []string{
		"id", "object", "created", "model", "system_fingerprint", "service_tier",
		"choices.0.index", "choices.0.finish_reason",
		"usage.prompt_tokens", "usage.completion_tokens", "usage.total_tokens",
	}
	src := gjson.Parse(everyContentChannel)
	for _, f := range envelope {
		before, after := src.Get(f), gjson.GetBytes(out, f)
		if before.Raw != after.Raw {
			t.Errorf("%s changed across a content redaction: %s → %s. Compliance scans content; "+
				"anything it rewrites outside content is a field a redaction silently deletes.",
				f, before.Raw, after.Raw)
		}
	}
}

// TestRewriteRefusesAPayloadFromAnotherBody pins the fail-closed arm: writing a
// prefix of a mismatched payload is how one channel's redaction lands on
// another channel's text.
func TestRewriteRefusesAPayloadFromAnotherBody(t *testing.T) {
	p := decodeEveryChannel(t)
	// Drop the reasoning block, as a payload decoded from a body without one
	// would have. The walk must refuse rather than shift everything up.
	for mi := range p.Messages {
		kept := p.Messages[mi].Content[:0]
		for _, b := range p.Messages[mi].Content {
			if b.Type != core.ContentReasoning {
				kept = append(kept, b)
			}
		}
		p.Messages[mi].Content = kept
	}
	if _, _, err := RewriteCanonicalResponseContent([]byte(everyContentChannel), p); err == nil {
		t.Error("rewrite accepted a payload with one channel missing — it would have written the " +
			"content block's text into the reasoning slot and shifted every later channel")
	}
}

// TestRewriteRefusesBlocksInTheWrongOrder is the gate that actually binds the
// type check.
//
// The first version of these tests used a fixture whose decode order happened
// to equal the wire walk order, so a rewrite matching blocks BY POSITION passed
// every one of them — including the "refuses a foreign payload" case above,
// which still errored, only for the unrelated reason that it ran out of blocks.
// A guard that never sees a divergence is not being tested.
//
// So: same blocks, same count, two of them swapped. Position-matching writes
// each channel's text into the other's slot and reports success; matching on
// the block's type refuses.
func TestRewriteRefusesBlocksInTheWrongOrder(t *testing.T) {
	p := decodeEveryChannel(t)

	ri, ci := -1, -1
	for mi := range p.Messages {
		for bi, b := range p.Messages[mi].Content {
			switch b.Type {
			case core.ContentReasoning:
				ri = bi
			case core.ContentText:
				if ci < 0 {
					ci = bi
				}
			}
		}
		if ri >= 0 && ci >= 0 {
			p.Messages[mi].Content[ri], p.Messages[mi].Content[ci] =
				p.Messages[mi].Content[ci], p.Messages[mi].Content[ri]
			break
		}
	}
	if ri < 0 || ci < 0 {
		t.Fatal("the fixture must decode both a reasoning and a text block, or this gate means nothing")
	}

	out, _, err := RewriteCanonicalResponseContent([]byte(everyContentChannel), p)
	if err == nil {
		t.Errorf("rewrite accepted blocks in the wrong order and produced:\n%s\n"+
			"Matching a wire slot to whatever block comes next is how a redacted chain-of-thought "+
			"gets delivered as the answer. The slot's channel and the block's type must agree.",
			string(out))
	}
}
