// Package canonicalbridge — non-stream response identity across the canonical waist.
//
// Named failure modes:
//   - a channel the upstream sent does not reach the canonical body
//   - a channel reaches canonical but not the re-encoded ingress wire
//   - a channel's text lands on a DIFFERENT channel (reasoning rendered as the answer)
package canonicalbridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The non-stream half of the chain the streaming suite already guards:
//
//	upstream response → codec DecodeResponse → canonical → ResponseCanonicalToIngress → wire
//
// It is the path an enforcing response scope takes when it buffers, and the one
// every cross-spec non-stream call takes. The gate that covered it asserted
// shape markers only — that `type` reads "message", that a `content` key
// exists — so a codec encoding content as an EMPTY ARRAY passed. What follows
// compares the channels themselves.
//
// The corpus is real upstream traffic under testdata/upstream-responses,
// captured with its request body beside it.

const responseCorpusDir = "testdata/upstream-responses"

type responseIdentityCase struct {
	corpus   string
	format   provcore.Format
	endpoint typology.WireShape
	// extract reads this provider's wire into named channels.
	extract func(body []byte) map[string]string
	// mountedIngress is true when a client can actually speak this wire TO the
	// gateway (cmd/ai-gateway/wiring/routes.go). Only those get the full
	// round-trip assertion; the rest are targets only, and for them the bar is
	// that the DECODE into canonical loses nothing — canonical being what
	// response hooks read and what the caller's own wire is rebuilt from.
	mountedIngress bool
}

func responseIdentityCases() []responseIdentityCase {
	return []responseIdentityCase{
		{"ns_openai_tools", provcore.FormatOpenAI, typology.WireShapeOpenAIChat, extractOpenAIResponseChannels, true},
		{"ns_openai_multichoice", provcore.FormatOpenAI, typology.WireShapeOpenAIChat, extractOpenAIResponseChannels, true},
		{"ns_openai_refusal", provcore.FormatOpenAI, typology.WireShapeOpenAIChat, extractOpenAIResponseChannels, true},
		{"ns_deepseek_reasoning", provcore.FormatDeepSeek, typology.WireShapeOpenAIChat, extractOpenAIResponseChannels, true},
		{"ns_moonshot_tools", provcore.FormatMoonshot, typology.WireShapeOpenAIChat, extractOpenAIResponseChannels, true},
		{"ns_anthropic_thinking_tools", provcore.FormatAnthropic, typology.WireShapeAnthropicMessages, extractAnthropicResponseChannels, true},
		// Gemini IS a mounted ingress — routes_native.go serves
		// POST /v1beta/models/{model} — so it gets the full round trip.
		{"ns_gemini_tools", provcore.FormatGemini, typology.WireShapeGeminiGenerateContent, extractGeminiResponseChannels, true},
		// Cohere serves as a TARGET for chat. Only /v1/rerank is mounted with a
		// Cohere wire shape, so no client speaks Cohere chat TO the gateway and
		// the bar here is the decode into canonical.
		{"ns_cohere_tools", provcore.FormatCohere, typology.WireShapeCohereChat, extractCohereResponseChannels, false},
		{"ns_cohere_reasoning", provcore.FormatCohere, typology.WireShapeCohereChat, extractCohereResponseChannels, false},
	}
}

func TestNonStreamResponseIsIdentityAcrossCanonical(t *testing.T) {
	b := testBridge(t)
	for _, tc := range responseIdentityCases() {
		t.Run(tc.corpus, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(responseCorpusDir, tc.corpus+".response.json"))
			if err != nil {
				t.Fatalf("read corpus: %v", err)
			}

			canonical, _, err := b.DecodeViaShared(tc.format, tc.endpoint, raw)
			if err != nil {
				t.Fatalf("DecodeViaShared: %v", err)
			}

			// The decode is asserted for every provider, on the canonical wire's
			// own reading: canonical IS the OpenAI shape, so a channel missing
			// here is one no response hook can scan and no egress can rebuild.
			assertCanonicalCarries(t, tc.extract(raw), extractOpenAIResponseChannels(canonical))

			if !tc.mountedIngress {
				return
			}
			wire, err := b.ResponseCanonicalToIngress(tc.format, canonical)
			if err != nil {
				t.Fatalf("ResponseCanonicalToIngress: %v", err)
			}

			before, after := tc.extract(raw), tc.extract(wire)
			names := make([]string, 0, len(before))
			for n := range before {
				names = append(names, n)
			}
			for n := range after {
				if _, seen := before[n]; !seen {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			if len(names) == 0 {
				t.Fatal("the extractor found no channels at all — a pass here would mean nothing")
			}

			for _, name := range names {
				got, want := after[name], before[name]
				if got == want {
					continue
				}
				switch {
				case want != "" && got == "":
					t.Errorf("%s: upstream sent %d bytes, re-encode carries none", name, len(want))
				case want == "" && got != "":
					t.Errorf("%s: upstream sent nothing, re-encode invents %q — text moved onto a "+
						"channel the model never used", name, clipResp(got))
				default:
					t.Errorf("%s: re-encode differs\n  upstream:  %q\n  re-encode: %q",
						name, clipResp(want), clipResp(got))
				}
			}
		})
	}
}

// TestChatCanonicalToResponsesWireCarriesEveryChannel covers the one conversion
// a non-stream /v1/responses request actually performs.
//
// When the upstream answers in the Responses grammar the proxy forwards those
// bytes verbatim — no canonical body is built, so there is nothing to lose. The
// conversion happens on the other branch: a chat-shaped upstream (any
// OpenAI-compatible target that does not serve /v1/responses) is canonical
// already and gets re-encoded into the output[] grammar. That is the path this
// asserts, using chat captures as the input they really are.
//
// An earlier version of this file ran the Responses captures through
// DecodeViaShared and reported four channels missing. That was the harness:
// FormatOpenAIResponses registers no codec, so DecodeViaShared returns the body
// untouched and a chat-shaped reader finds nothing in it. The failure was real
// output from a probe pointed at a path production does not take.
func TestChatCanonicalToResponsesWireCarriesEveryChannel(t *testing.T) {
	b := testBridge(t)
	for _, corpus := range []string{"ns_openai_tools", "ns_openai_refusal", "ns_deepseek_reasoning"} {
		t.Run(corpus, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(responseCorpusDir, corpus+".response.json"))
			if err != nil {
				t.Fatalf("read corpus: %v", err)
			}
			wire, err := b.ResponseCanonicalToIngress(provcore.FormatOpenAIResponses, raw)
			if err != nil {
				t.Fatalf("ResponseCanonicalToIngress(responses): %v", err)
			}

			chat := extractOpenAIResponseChannels(raw)
			responses := extractResponsesResponseChannels(wire)
			joined := strings.Join(mapValues(responses), "\x00")
			for _, ch := range []string{"content", "reasoning", "refusal", "tool_name", "tool_arguments"} {
				want := chat[ch]
				if want == "" {
					continue
				}
				if !strings.Contains(joined, want) {
					t.Errorf("%s: absent from the /v1/responses egress — a client on that wire "+
						"receives less than the upstream sent; chat body had %q", ch, clipResp(want))
				}
			}
		})
	}
}

// TestNonStreamCrossSpecMatrix walks every mounted ingress against every
// captured target, because smart routing can send any request to any model.
//
// ingress == target is the rare case, not the common one: a caller asks for a
// model and the router picks a provider, so an OpenAI-shaped request routinely
// comes back from Anthropic, a /v1/responses request from Gemini, a Gemini
// request from DeepSeek. Each of those pairs rebuilds the caller's wire from the
// canonical body, and each is a place a channel can vanish.
//
// The bar here is survival rather than equality — two different wires cannot be
// identical, and a channel the target wire cannot express is a declared trade
// (there are none for the text channels asserted here). Tool ARGUMENTS are left
// out: the egress shapes disagree about whether they are a JSON string or an
// object, which is translation, not loss.
func TestNonStreamCrossSpecMatrix(t *testing.T) {
	b := testBridge(t)
	// Ingresses a client can actually speak to this gateway (routes.go +
	// routes_native.go). Azure and GLM are mounted too and carry the OpenAI chat
	// shape, so FormatOpenAI stands for all three.
	ingresses := []struct {
		name string
		f    provcore.Format
	}{
		{"openai_chat", provcore.FormatOpenAI},
		{"responses", provcore.FormatOpenAIResponses},
		{"anthropic_messages", provcore.FormatAnthropic},
		{"gemini_generate", provcore.FormatGemini},
	}
	readers := map[provcore.Format]func([]byte) map[string]string{
		provcore.FormatOpenAI:          extractOpenAIResponseChannels,
		provcore.FormatOpenAIResponses: extractResponsesResponseChannels,
		provcore.FormatAnthropic:       extractAnthropicResponseChannels,
		provcore.FormatGemini:          extractGeminiResponseChannels,
	}

	for _, tc := range responseIdentityCases() {
		if tc.corpus == "ns_openai_multichoice" {
			// n>1 has no counterpart on the other wires: an Anthropic message, a
			// Gemini candidate list the encoder fills with one entry, and a
			// Responses output[] all carry ONE assistant turn. Only the first
			// candidate can survive, and that is the wire's limit rather than a
			// codec dropping something it could have carried. Same-format keeps
			// asserting all of it.
			continue
		}
		raw, err := os.ReadFile(filepath.Join(responseCorpusDir, tc.corpus+".response.json"))
		if err != nil {
			t.Fatalf("read corpus %s: %v", tc.corpus, err)
		}
		canonical, _, err := b.DecodeViaShared(tc.format, tc.endpoint, raw)
		if err != nil {
			t.Fatalf("%s: DecodeViaShared: %v", tc.corpus, err)
		}
		upstream := tc.extract(raw)

		for _, ing := range ingresses {
			if ing.f == tc.format {
				continue // same-format is TestNonStreamResponseIsIdentityAcrossCanonical
			}
			t.Run(tc.corpus+"_to_"+ing.name, func(t *testing.T) {
				wire, err := b.ResponseCanonicalToIngress(ing.f, canonical)
				if err != nil {
					t.Fatalf("ResponseCanonicalToIngress(%s): %v", ing.f, err)
				}
				joined := strings.Join(mapValues(readers[ing.f](wire)), "\x00")
				for _, ch := range []string{"content", "reasoning", "refusal", "tool_name"} {
					want := upstream[ch]
					if want == "" {
						continue
					}
					if !strings.Contains(joined, want) {
						t.Errorf("%s: absent from the %s egress — smart routing can land this pair, "+
							"and a client on that wire would receive less than the upstream sent; "+
							"upstream had %q", ch, ing.name, clipResp(want))
					}
				}
			})
		}
	}
}

// assertCanonicalCarries checks the decode half: every channel the upstream sent
// must be readable off the canonical body.
//
// It compares CONTENT rather than exact equality per channel, because canonical
// is a different wire from the provider's and may legitimately place a value
// elsewhere — Anthropic's stop_reason becomes an OpenAI finish_reason with a
// different vocabulary, and tool arguments become a JSON string where the
// provider sent an object. What must not happen is the text going missing.
func assertCanonicalCarries(t *testing.T, upstream, canonical map[string]string) {
	t.Helper()
	joined := strings.Join(mapValues(canonical), "\x00")
	for _, ch := range []string{"content", "reasoning", "refusal", "tool_name", "tool_plan"} {
		want := upstream[ch]
		if want == "" {
			continue
		}
		if !strings.Contains(joined, want) {
			t.Errorf("canonical body is missing the %s channel — response hooks read canonical, "+
				"so a channel absent here is one no rule can scan and no egress can rebuild; "+
				"upstream sent %q", ch, clipResp(want))
		}
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// extractOpenAIResponseChannels folds every choice, so an n>1 answer cannot pass
// by having its first candidate survive.
func extractOpenAIResponseChannels(body []byte) map[string]string {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				Refusal          string `json:"refusal"`
				ToolCalls        []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	out := newChannelSet()
	if json.Unmarshal(body, &resp) != nil {
		return out.flat()
	}
	for _, c := range resp.Choices {
		out.add("content", c.Message.Content)
		reasoning := c.Message.ReasoningContent
		if reasoning == "" {
			reasoning = c.Message.Reasoning
		}
		out.add("reasoning", reasoning)
		out.add("refusal", c.Message.Refusal)
		for _, tc := range c.Message.ToolCalls {
			out.add("tool_name", tc.Function.Name)
			out.add("tool_arguments", tc.Function.Arguments)
		}
		out.add("finish_reason", c.FinishReason)
	}
	return out.flat()
}

func extractAnthropicResponseChannels(body []byte) map[string]string {
	var resp struct {
		Content []struct {
			Type     string         `json:"type"`
			Text     string         `json:"text"`
			Thinking string         `json:"thinking"`
			Name     string         `json:"name"`
			Input    map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	out := newChannelSet()
	if json.Unmarshal(body, &resp) != nil {
		return out.flat()
	}
	for _, block := range resp.Content {
		out.add("content", block.Text)
		out.add("reasoning", block.Thinking)
		out.add("tool_name", block.Name)
		if block.Input != nil {
			if enc, err := json.Marshal(block.Input); err == nil && string(enc) != "null" {
				out.add("tool_arguments", string(enc))
			}
		}
	}
	out.add("finish_reason", resp.StopReason)
	return out.flat()
}

func extractGeminiResponseChannels(body []byte) map[string]string {
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					Thought      bool   `json:"thought"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	out := newChannelSet()
	if json.Unmarshal(body, &resp) != nil {
		return out.flat()
	}
	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.Thought {
				out.add("reasoning", part.Text)
			} else {
				out.add("content", part.Text)
			}
			if part.FunctionCall != nil {
				out.add("tool_name", part.FunctionCall.Name)
				if enc, err := json.Marshal(part.FunctionCall.Args); err == nil && string(enc) != "null" {
					out.add("tool_arguments", string(enc))
				}
			}
		}
		out.add("finish_reason", cand.FinishReason)
	}
	return out.flat()
}

// extractResponsesResponseChannels reads the /v1/responses output[] grammar,
// where each item declares its own type: a function_call carries its name, a
// reasoning item its summary, a message item the answer.
func extractResponsesResponseChannels(body []byte) map[string]string {
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Args    string `json:"arguments"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
				// A decline is its own content part on this wire, carrying the
				// text under `refusal` rather than `text`. Reading only `text`
				// reported the channel as dropped when the encoder was emitting
				// it correctly.
				Refusal string `json:"refusal"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Status string `json:"status"`
	}
	out := newChannelSet()
	if json.Unmarshal(body, &resp) != nil {
		return out.flat()
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "function_call":
			out.add("tool_name", item.Name)
			out.add("tool_arguments", item.Args)
		case "reasoning":
			for _, s := range item.Summary {
				out.add("reasoning", s.Text)
			}
			for _, c := range item.Content {
				out.add("reasoning", c.Text)
			}
		case "message":
			for _, c := range item.Content {
				out.add("content", c.Text)
				out.add("refusal", c.Refusal)
			}
		}
	}
	out.add("finish_reason", resp.Status)
	return out.flat()
}

func extractCohereResponseChannels(body []byte) map[string]string {
	var resp struct {
		Message struct {
			ToolPlan string `json:"tool_plan"`
			Content  []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	out := newChannelSet()
	if json.Unmarshal(body, &resp) != nil {
		return out.flat()
	}
	for _, block := range resp.Message.Content {
		out.add("content", block.Text)
		out.add("reasoning", block.Thinking)
	}
	out.add("tool_plan", resp.Message.ToolPlan)
	for _, tc := range resp.Message.ToolCalls {
		out.add("tool_name", tc.Function.Name)
		out.add("tool_arguments", tc.Function.Arguments)
	}
	out.add("finish_reason", resp.FinishReason)
	return out.flat()
}

// channelSet accumulates one string per channel in arrival order.
type channelSet struct{ m map[string]*strings.Builder }

func newChannelSet() *channelSet { return &channelSet{m: map[string]*strings.Builder{}} }

func (c *channelSet) add(key, value string) {
	if value == "" {
		return
	}
	b, ok := c.m[key]
	if !ok {
		b = &strings.Builder{}
		c.m[key] = b
	}
	b.WriteString(value)
}

func (c *channelSet) flat() map[string]string {
	out := make(map[string]string, len(c.m))
	for k, b := range c.m {
		out[k] = b.String()
	}
	return out
}

func clipResp(s string) string {
	if len(s) <= 56 {
		return s
	}
	return s[:56] + "…"
}
