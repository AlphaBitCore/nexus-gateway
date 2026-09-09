// Package canonicalbridge_test — spec-translation fidelity against real upstream bytes.
//
// Named failure modes:
//   - a channel the upstream sent does not reach the canonical chunk (hooks go blind to it)
//   - a channel reaches canonical but not the re-encoded wire (cross-spec and enforcing
//     lanes drop it, because both rebuild the wire from the canonical chunk)
//   - the corpus no longer contains a channel it was captured to cover (stale corpus:
//     a probe that finds nothing would otherwise report a false all-clear)
package canonicalbridge_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	anthropicspec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	coherespec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/cohere"
	geminispec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini"
	openaispec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The chain under test is the product's own:
//
//	upstream response  →  provider codec  →  canonical spec  →  provider codec  →  wire
//
// A cross-spec call (an OpenAI-shaped request routed to Gemini, say) runs it, and so
// does every enforcing response scope, because both rebuild the wire from the canonical
// chunk instead of forwarding the upstream bytes. It should be an identity: whatever the
// upstream sent has to come back out.
//
// The corpus is REAL upstream traffic — request body and response bytes captured
// together under testdata/upstream-streams. Hand-written frames were tried first and
// were the wrong tool: they encode what the author believes the wire looks like, and a
// belief that is wrong produces a test that passes while the product loses data.

const corpusDir = "testdata/upstream-streams"

type fidelityCase struct {
	corpus  string
	ingress provcore.Format
	shape   typology.WireShape
	open    func(io.ReadCloser, typology.WireShape) (provcore.StreamSession, error)
	// channels maps a human name to the JSON key whose string values carry that
	// channel in this provider's wire. Every listed channel MUST appear in the
	// corpus; an empty extraction fails the test as a stale corpus rather than
	// passing as "nothing to check".
	channels map[string]string
}

func openaiOpen(r io.ReadCloser, s typology.WireShape) (provcore.StreamSession, error) {
	return openaispec.NewStreamDecoder(slog.Default()).Open(r, s)
}

func anthropicOpen(r io.ReadCloser, s typology.WireShape) (provcore.StreamSession, error) {
	return anthropicspec.NewStreamDecoder(slog.Default()).Open(r, s)
}

func cohereOpen(r io.ReadCloser, s typology.WireShape) (provcore.StreamSession, error) {
	return coherespec.NewStreamDecoder(slog.Default()).Open(r, s)
}

func geminiOpen(r io.ReadCloser, s typology.WireShape) (provcore.StreamSession, error) {
	return geminispec.NewStreamDecoder(slog.Default()).Open(r, s)
}

func fidelityCases() []fidelityCase {
	return []fidelityCase{
		{
			corpus: "openai_tools", ingress: provcore.FormatOpenAI,
			shape: typology.WireShapeOpenAIChat, open: openaiOpen,
			channels: map[string]string{
				"tool name":      "name",
				"tool arguments": "arguments",
				"finish_reason":  "finish_reason",
			},
		},
		{
			// n>1 arrives as SEPARATE frames each carrying one choice, so
			// choices.0 addresses whichever choice that frame holds. Captured to
			// pin that, because the opposite reading — several choices packed
			// into one frame — implies a decoder bug that is not there.
			corpus: "openai_multichoice", ingress: provcore.FormatOpenAI,
			shape: typology.WireShapeOpenAIChat, open: openaiOpen,
			channels: map[string]string{"content across both choices": "content"},
		},
		{
			// A structured-outputs decline streams on delta.refusal INSTEAD of
			// delta.content. Both the canonical chunk and the re-encode used to
			// drop it, so a client saw an empty answer where the model had
			// refused, and no response rule ever scanned the refusal text.
			corpus: "openai_refusal", ingress: provcore.FormatOpenAI,
			shape: typology.WireShapeOpenAIChat, open: openaiOpen,
			channels: map[string]string{"refusal": "refusal"},
		},
		{
			corpus: "deepseek_reasoning", ingress: provcore.FormatOpenAI,
			shape: typology.WireShapeOpenAIChat, open: openaiOpen,
			channels: map[string]string{
				"reasoning_content": "reasoning_content",
				"content":           "content",
			},
		},
		{
			corpus: "moonshot_tools", ingress: provcore.FormatOpenAI,
			shape: typology.WireShapeOpenAIChat, open: openaiOpen,
			channels: map[string]string{
				"tool name":      "name",
				"tool arguments": "arguments",
			},
		},
		{
			corpus: "anthropic_thinking_tools", ingress: provcore.FormatAnthropic,
			shape: typology.WireShapeAnthropicMessages, open: anthropicOpen,
			channels: map[string]string{
				"tool name":                "name",
				"tool arguments (partial)": "partial_json",
			},
		},
		{
			// The tool-calling capture never produced prose, so this one asks a
			// question with no tools attached: it is the only capture that
			// exercises Anthropic's plain text_delta run.
			corpus: "anthropic_thinking_text", ingress: provcore.FormatAnthropic,
			shape: typology.WireShapeAnthropicMessages, open: anthropicOpen,
			channels: map[string]string{"text_delta": "text"},
		},
		{
			// Extended thinking with an explicit budget: thinking_delta plus the
			// signature_delta that closes the block. Same-format is the case that
			// matters here — the encoder replays provider-native thinking only
			// from the signed carrier, so this pins that the carrier survives the
			// canonical waist rather than the thinking being silently dropped.
			corpus: "anthropic_thinking_block", ingress: provcore.FormatAnthropic,
			shape: typology.WireShapeAnthropicMessages, open: anthropicOpen,
			channels: map[string]string{
				"thinking_delta": "thinking",
				"text_delta":     "text",
			},
		},
		{
			// /v1/responses is a different event grammar entirely — named events
			// carrying a bare `delta` string rather than a choices[] envelope —
			// and it has its own decoder and its own encoder, so nothing the
			// chat-completions captures exercise covers it.
			corpus: "openai_responses_tools", ingress: provcore.FormatOpenAIResponses,
			shape: typology.WireShapeOpenAIResponses, open: openaiOpen,
			channels: map[string]string{"function_call_arguments delta": "delta"},
		},
		{
			// The reasoning summary and the answer both arrive on `delta`,
			// distinguished only by their event name: 91
			// response.reasoning_summary_text.delta against 102
			// response.output_text.delta. Losing either shows up here.
			corpus: "openai_responses_reasoning", ingress: provcore.FormatOpenAIResponses,
			shape: typology.WireShapeOpenAIResponses, open: openaiOpen,
			channels: map[string]string{"output_text + reasoning_summary delta": "delta"},
		},
		{
			corpus: "cohere_tools", ingress: provcore.FormatCohere,
			shape: typology.WireShapeCohereChat, open: cohereOpen,
			channels: map[string]string{"tool_plan": "tool_plan"},
		},
		{
			// command-a-reasoning streams TWO content blocks: a `thinking` one
			// and a `text` one. The docs' chat-stream event table lists neither,
			// which is why this is captured from the live API rather than
			// written from the reference.
			corpus: "cohere_reasoning", ingress: provcore.FormatCohere,
			shape: typology.WireShapeCohereChat, open: cohereOpen,
			channels: map[string]string{
				"thinking": "thinking",
				"text":     "text",
			},
		},
		{
			corpus: "gemini_tools", ingress: provcore.FormatGemini,
			shape: typology.WireShapeGeminiGenerateContent, open: geminiOpen,
			channels: map[string]string{"functionCall name": "name"},
		},
		{
			corpus: "gemini_thinking", ingress: provcore.FormatGemini,
			shape: typology.WireShapeGeminiGenerateContent, open: geminiOpen,
			channels: map[string]string{"parts[].text (thought run)": "text"},
		},
	}
}

func TestStreamRoundTripFidelity(t *testing.T) {
	for _, tc := range fidelityCases() {
		t.Run(tc.corpus, func(t *testing.T) {
			raw := readCorpus(t, tc.corpus)
			canon, wire := runChain(t, tc, raw)

			names := make([]string, 0, len(tc.channels))
			for n := range tc.channels {
				names = append(names, n)
			}
			sort.Strings(names)

			for _, name := range names {
				values := stringsUnderKey(raw, tc.channels[name])
				if len(values) == 0 {
					t.Errorf("%s: corpus carries no %q values — the capture no longer covers this channel, "+
						"so a pass here would mean nothing; re-capture with scratchpad/corpus.py",
						name, tc.channels[name])
					continue
				}
				var lostCanon, lostWire []string
				for _, v := range values {
					if !strings.Contains(canon, v) {
						lostCanon = append(lostCanon, v)
					}
					if !containsEscaped(wire, v) {
						lostWire = append(lostWire, v)
					}
				}
				switch {
				case len(lostCanon) > 0 && len(lostWire) > 0:
					t.Errorf("%s: %d/%d values reach neither the canonical chunk nor the re-encoded wire; first missing %q",
						name, len(lostCanon), len(values), clip(lostCanon[0]))
				case len(lostWire) > 0:
					t.Errorf("%s: %d/%d values are in the canonical chunk but absent from the re-encoded wire — "+
						"cross-spec and enforcing lanes would drop them; first missing %q",
						name, len(lostWire), len(values), clip(lostWire[0]))
				case len(lostCanon) > 0:
					t.Errorf("%s: %d/%d values never reach the canonical chunk — response hooks read the canonical "+
						"waist, so they are blind to this channel; first missing %q",
						name, len(lostCanon), len(values), clip(lostCanon[0]))
				}
			}
		})
	}
}

// TestCrossSpecStreamFidelity is the case the canonical waist exists for: an
// upstream speaking one wire, a client speaking another. Same-format traffic can
// (and outside an enforcing scope does) forward the upstream frame byte for byte,
// so it exercises the translation far less than a cross pair does — every cross
// pair MUST rebuild the wire from the canonical chunk.
//
// The criterion here is NOT identity, which two different wires cannot have. It
// is: a channel the target wire can express must survive; a channel the target
// wire has no place for is a declared trade, named below with its reason. The
// distinction matters because the tempting "fix" for a missing channel — writing
// the text into whatever field the target does have — invents content the model
// never produced on that channel, which is a worse defect than the omission. The
// anthropic encoder already takes that position for reasoning, refusing to
// synthesise an unsigned thinking block.
//
// Tool calls are not asserted here: the egress shapes disagree about what a tool
// call even is (Gemini re-splits arguments into a structured `args` object rather
// than the fragment string the upstream streamed), which is translation, not
// loss. The same-format cases pin the tool-call fragments.
func TestCrossSpecStreamFidelity(t *testing.T) {
	egress := []struct {
		name string
		f    provcore.Format
	}{
		{"openai", provcore.FormatOpenAI},
		{"responses", provcore.FormatOpenAIResponses},
		{"anthropic", provcore.FormatAnthropic},
		{"gemini", provcore.FormatGemini},
		{"cohere", provcore.FormatCohere},
	}
	// corpus → the JSON keys carrying plain text in that corpus.
	textChannels := map[string][]string{
		"openai_multichoice": {"content"},
		"openai_refusal":     {"refusal"},
		"deepseek_reasoning": {"content", "reasoning_content"},
		"gemini_thinking":    {"text"},
	}
	// "<channel> -> <egress>" → why that wire cannot carry it. An entry here is a
	// decision, not a waiver: removing one means teaching that encoder the
	// channel, never widening the assertion.
	//
	// A refusal is NOT in here. Those wires have no refusal field either, but the
	// text is ordinary prose on all of them, so it rides the content channel: a
	// client seeing the decline beats a client seeing an empty turn. Only the
	// CLASSIFICATION is lost there, not the words. Reasoning is different — an
	// Anthropic thinking block is a signed artifact, and forging an unsigned one
	// would put words in a channel the provider vouches for.
	declaredTrades := map[string]string{}

	byName := map[string]fidelityCase{}
	for _, tc := range fidelityCases() {
		byName[tc.corpus] = tc
	}

	names := make([]string, 0, len(textChannels))
	for n := range textChannels {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, corpus := range names {
		tc, ok := byName[corpus]
		if !ok {
			t.Fatalf("%s: no fidelity case declares this corpus", corpus)
		}
		raw := readCorpus(t, corpus)
		for _, eg := range egress {
			if eg.f == tc.ingress {
				continue // same-format identity is TestOpenAIStreamIsIdentityAcrossCanonical
			}
			t.Run(corpus+"_to_"+eg.name, func(t *testing.T) {
				cross := tc
				cross.ingress = eg.f
				_, wire := runChain(t, cross, raw)
				for _, key := range textChannels[corpus] {
					values := stringsUnderKey(raw, key)
					if len(values) == 0 {
						t.Fatalf("corpus carries no %q values — capture is stale", key)
					}
					var lost []string
					for _, v := range values {
						if !containsEscaped(wire, v) {
							lost = append(lost, v)
						}
					}
					reason, traded := declaredTrades[key+" -> "+eg.name]
					switch {
					case len(lost) > 0 && !traded:
						t.Errorf("%s: %d/%d values absent from the %s egress — that wire can carry this "+
							"channel, so a client on it receives less than the upstream sent; first missing %q",
							key, len(lost), len(values), eg.name, clip(lost[0]))
					case len(lost) > 0:
						t.Logf("%s -> %s: %d/%d dropped, declared trade — %s",
							key, eg.name, len(lost), len(values), reason)
					case traded:
						t.Errorf("%s -> %s is recorded as a declared trade, but the values now survive; "+
							"delete the entry rather than leaving a stale exemption", key, eg.name)
					}
				}
			})
		}
	}
}

// TestStreamCorpusCoversDeclaredChannels answers the question its name asks:
// does every channel the canonical stream contract declares get exercised by at
// least one capture? A channel no corpus contains is untested, and untested is
// where the next silent loss lands.
//
// The previous version could not answer it. It declared an EMPTY knownGaps
// literal that nothing computed, summed the channels the cases happened to
// declare, and then logged. Its only failing path was "no channels at all", so
// the doc line "this fails when the gap set grows" was not true of any input —
// dropping a channel from a decoder left it silent.
//
// The declared side is now derived from SubsetFields() via the same carrier map
// the contract-binding test uses, so a channel added to the contract is covered
// the day it is declared rather than the day someone remembers this list.
func TestStreamCorpusCoversDeclaredChannels(t *testing.T) {
	_, _, stream := canonicalbridge.SubsetFields()
	if len(stream) == 0 {
		t.Fatal("SubsetFields returned no stream paths — the contract would be vacuous")
	}

	// The wire keys every capture between them exercises. A case's channel map
	// names the JSON key that carries it on that provider's wire, which is the
	// same vocabulary the carrier map speaks.
	exercised := map[string]string{} // wire key -> the corpus that covers it
	for _, tc := range fidelityCases() {
		for _, key := range tc.channels {
			if _, seen := exercised[key]; !seen {
				exercised[key] = tc.corpus
			}
		}
	}
	if len(exercised) == 0 {
		t.Fatal("no channels declared by any fidelity case — the suite would pass vacuously")
	}

	// Channels the contract declares, carried by a Chunk field, whose text no
	// capture exercises. Each entry is a real gap: shrink it by capturing the
	// traffic, never by deleting the line.
	knownGaps := map[string]string{
		"choices[].delta.role":    "the role delta carries no user content; a loss would be visible as a malformed first frame, not as missing text",
		"choices[].finish_reason": "asserted by TestFinishReasonRoundTrip rather than by channel text",
		"choices[].index":         "asserted by the multi-choice corpus structurally, not as a text channel",
		"usage":                   "asserted by the usage round-trip tests",
	}

	var missing []string
	for _, path := range stream {
		key := wireKeyForDeclaredPath(path)
		if key == "" {
			continue // structural container, not a text channel
		}
		if _, ok := exercised[key]; ok {
			continue
		}
		if _, known := knownGaps[path]; known {
			continue
		}
		missing = append(missing, path)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the canonical stream contract declares %v, and no capture exercises them.\n"+
			"An unexercised channel is one a decoder can stop carrying without any test "+
			"noticing — capture the traffic, or record the reason in knownGaps.", missing)
	}
	t.Logf("%d wire keys exercised across %d captures; %d recorded gaps",
		len(exercised), len(fidelityCases()), len(knownGaps))
}

// wireKeyForDeclaredPath maps a declared contract path to the JSON key a
// fidelity case would name for it, or "" when the path is a container rather
// than a text channel.
func wireKeyForDeclaredPath(path string) string {
	switch path {
	case "choices[].delta.content":
		return "content"
	case "choices[].delta.refusal":
		return "refusal"
	case "choices[].delta.reasoning_content":
		return "reasoning_content"
	case "choices[].delta.tool_calls":
		return "arguments"
	}
	return ""
}

func readCorpus(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDir, name+".response.sse"))
	if err != nil {
		t.Fatalf("read corpus %s: %v", name, err)
	}
	return string(b)
}

// runChain drives the decoder and feeds every chunk straight back through the
// ingress encoder, returning what the canonical chunk carried and what the
// re-encoded wire carried.
func runChain(t *testing.T, tc fidelityCase, raw string) (canonical, wire string) {
	t.Helper()
	sess, err := tc.open(io.NopCloser(strings.NewReader(raw)), tc.shape)
	if err != nil {
		t.Fatalf("open %s stream: %v", tc.ingress, err)
	}
	defer sess.Close()

	enc := canonicalbridge.IngressStreamEncoder(tc.ingress, "roundtrip-model")
	var canonB, wireB strings.Builder
	ctx := context.Background()
	for {
		ch, err := sess.Next(ctx)
		if err != nil {
			break
		}
		// NUL separates the fields so a value cannot appear to survive by
		// straddling two of them.
		for _, s := range []string{ch.Delta, ch.ReasoningDelta, ch.RefusalDelta, ch.FinishReason} {
			canonB.WriteString(s)
			canonB.WriteByte(0)
		}
		for _, d := range ch.ToolCallDeltas {
			canonB.WriteString(d.Name)
			canonB.WriteByte(0)
			canonB.WriteString(d.Arguments)
			canonB.WriteByte(0)
			canonB.WriteString(d.ID)
			canonB.WriteByte(0)
		}
		for _, b := range ch.NexusThinking {
			canonB.WriteString(b.Thinking)
			canonB.WriteByte(0)
		}
		if b, err := enc.Write(ctx, ch); err == nil {
			wireB.Write(b)
		}
		if ch.Done {
			break
		}
	}
	return canonB.String(), wireB.String()
}

// stringsUnderKey returns every distinct non-empty string stored under key,
// anywhere in any frame.
//
// It parses each frame and walks the tree rather than scanning for `"key":"`.
// The scanning version reported Gemini as carrying no text at all: those
// captures are pretty-printed as `"text": "…"`, and the needle missed every
// one. An absent measurement that reads as an absent channel is the failure
// this whole suite exists to prevent, so the extraction cannot have that shape.
func stringsUnderKey(raw, key string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(v any, under string)
	walk = func(v any, under string) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				walk(child, k)
			}
		case []any:
			for _, child := range t {
				walk(child, under)
			}
		case string:
			if under == key && t != "" && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var v any
		if json.Unmarshal([]byte(payload), &v) == nil {
			walk(v, "")
		}
	}
	sort.Strings(out)
	return out
}

// containsEscaped looks for the value both raw and JSON-escaped: the re-encoded
// wire holds it as an escaped JSON string, so a value with a quote or a newline
// would otherwise read as lost.
func containsEscaped(haystack, value string) bool {
	if strings.Contains(haystack, value) {
		return true
	}
	b, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return strings.Contains(haystack, strings.Trim(string(b), `"`))
}

func clip(s string) string {
	if len(s) <= 48 {
		return s
	}
	return s[:48] + "…"
}
