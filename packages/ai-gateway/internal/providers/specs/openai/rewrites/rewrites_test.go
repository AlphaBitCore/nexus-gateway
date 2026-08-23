// Package rewrites_test pins the OpenAI quirk-table boundaries. Named
// failure modes per provider-adapter-architecture.md §3a Rule 3/7:
//   - gpt-5.4* accepts sampling params (probed 200 on both wires) but
//     still needs the max_tokens rename — the two predicates split there
//   - gpt-5.5 / gpt-5.6* / o-series reject sampling params AND max_tokens
//   - an unprobed future gpt-5.x defaults to the strip (fail safe)
//   - classic models (gpt-4o, gpt-3.5-turbo) match neither predicate
//   - the responses rule list carries the strips but never the rename
package rewrites_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/rewrites"
)

func TestRejectsSamplingParams_Boundaries(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		// Probed 400s: strip.
		{"gpt-5.5", true},
		{"gpt-5.6-luna", true},
		{"gpt-5.6-sol", true},
		{"o1", true},
		{"o1-mini", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4-mini", true},
		// Unprobed gpt-5 families: fail safe, strip.
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5.9", true},
		// Generations past 5 that do not exist yet. The rejecting set is
		// "generation >= 5", not the literal "gpt-5" string: a lineage nobody
		// has probed must land INSIDE the strip, or its first caller pays the
		// 400 (kimi-k2.7 reached callers exactly that way).
		{"gpt-6", true},
		{"gpt-6-mini", true},
		{"gpt-6.2-nova", true},
		{"gpt-7", true},
		{"gpt-12", true},
		// Probed 200 on both wires: the gpt-5.4 carve-out.
		{"gpt-5.4", false},
		{"gpt-5.4-mini", false},
		{"gpt-5.4-nano", false},
		// Version boundary: a hypothetical gpt-5.40 is NOT gpt-5.4 and must
		// not inherit the probed ACCEPT (the fail-unsafe direction) — it
		// falls back to the unprobed-gpt-5.x fail-safe strip.
		{"gpt-5.40", true},
		// Not reasoning families at all.
		{"gpt-4o", false},
		{"gpt-4.1", false},
		{"gpt-3.5-turbo", false},
		{"openai-gpt4", false},
		{"claude-sonnet-4-6", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := rewrites.RejectsSamplingParams(tc.model); got != tc.want {
			t.Errorf("RejectsSamplingParams(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestNeedsMaxTokensRename_Boundaries(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		// The rename covers ALL gpt-5* including the gpt-5.4 carve-out
		// (probed: gpt-5.4 400s on max_tokens) and the o-series.
		{"gpt-5.4", true},
		{"gpt-5.4-mini", true},
		{"gpt-5.5", true},
		{"gpt-5.6-luna", true},
		{"gpt-5", true},
		{"o1", true},
		{"o3-mini", true},
		{"o4-mini", true},
		// Same generation test as the sampling strip: an unreleased gpt-6 is
		// the same reasoning lineage and needs the same rename, or it 400s on
		// a parameter name we could have translated.
		{"gpt-6", true},
		{"gpt-6-mini", true},
		{"gpt-7.1", true},
		// Untouched families.
		{"gpt-4o", false},
		{"gpt-4.1", false},
		{"gpt-4.5-preview", false},
		{"openai-gpt4", false},
		{"gpt-", false},
		{"gpt-x", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := rewrites.NeedsMaxTokensRename(tc.model); got != tc.want {
			t.Errorf("NeedsMaxTokensRename(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestIsAda002Embedding_Boundaries(t *testing.T) {
	for model, want := range map[string]bool{
		"text-embedding-ada-002":    true,
		"text-embedding-ada-002-v2": true,
		"text-embedding-3-small":    false,
		"":                          false,
	} {
		if got := rewrites.IsAda002Embedding(model); got != want {
			t.Errorf("IsAda002Embedding(%q) = %v, want %v", model, got, want)
		}
	}
}

// TestOpenAIContract_Shape pins the table's wiring: which rules ride which
// wire, in which report order, gated by which predicate. The chat and
// responses lists share the same strip rules by construction, so the two
// wires cannot drift — the incident class this table exists for is a strip
// present on one wire and missing on the other.
func TestOpenAIContract_Shape(t *testing.T) {
	c := rewrites.OpenAIContract()

	chatWant := []struct{ field, renameTo, setRaw, cond string }{
		{"max_tokens", "max_completion_tokens", "", ""},
		{"temperature", "", "", ""},
		{"top_p", "", "", ""},
		{"reasoning_effort", "", `"none"`, "tools"},
		{"modalities", "", "", ""},
	}
	if len(c.Chat) != len(chatWant) {
		t.Fatalf("chat rules: got %d, want %d", len(c.Chat), len(chatWant))
	}
	for i, w := range chatWant {
		r := c.Chat[i]
		if r.Field != w.field || r.RenameTo != w.renameTo || r.SetRaw != w.setRaw || r.WhenPresentNonEmpty != w.cond {
			t.Errorf("chat[%d] = {%s→%s set=%s when=%s}, want {%s→%s set=%s when=%s}",
				i, r.Field, r.RenameTo, r.SetRaw, r.WhenPresentNonEmpty, w.field, w.renameTo, w.setRaw, w.cond)
		}
	}
	// The forced-effort rule is gpt-5.6-only (probed: gpt-5.5 and gpt-5.4
	// accept tools with the field absent) and must never leak onto the
	// responses wire — the vendor's own 400 points callers there.
	effort := c.Chat[3]
	for model, want := range map[string]bool{
		"gpt-5.6-terra": true, "gpt-5.6-luna": true, "gpt-5.6-sol": true, "gpt-5.6": true,
		// Version boundary: gpt-5.60 is a different family and must not
		// inherit the forced effort.
		"gpt-5.60": false,
		"gpt-5.5":  false, "gpt-5.4": false, "o3": false, "gpt-4o": false,
	} {
		if got := effort.Applies(model); got != want {
			t.Errorf("effort rule Applies(%q) = %v, want %v", model, got, want)
		}
	}
	for _, r := range c.Responses {
		if r.Field == "reasoning_effort" {
			t.Errorf("the forced-effort rule must not ride the responses wire (tools work there)")
		}
	}

	// The responses wire gets the sampling strips but never the rename:
	// /v1/responses carries the cap as max_output_tokens, so both chat
	// names are invalid there and renaming one rejected parameter to
	// another only changes which 400 the caller gets.
	if len(c.Responses) != 2 {
		t.Fatalf("responses rules: got %d, want 2", len(c.Responses))
	}
	for i, want := range []string{"temperature", "top_p"} {
		if c.Responses[i].Field != want || c.Responses[i].RenameTo != "" {
			t.Errorf("responses[%d] = {%s→%s}, want plain %s strip", i, c.Responses[i].Field, c.Responses[i].RenameTo, want)
		}
	}

	// Wire parity by construction: for every model, a responses strip
	// fires iff the chat strip for the same field fires.
	for _, model := range []string{"gpt-5.6-luna", "gpt-5.4", "o3", "gpt-4o", ""} {
		for _, respRule := range c.Responses {
			var chatGate bool
			for _, chatRule := range c.Chat {
				if chatRule.Field == respRule.Field {
					chatGate = chatRule.Applies(model)
				}
			}
			if respGate := respRule.Applies(model); respGate != chatGate {
				t.Errorf("%s/%s: responses gate %v != chat gate %v — the wires drifted", model, respRule.Field, respGate, chatGate)
			}
		}
	}

	// Embeddings: the ada-002 strips, labels carrying the family.
	if len(c.Embeddings) != 2 {
		t.Fatalf("embeddings rules: got %d, want 2", len(c.Embeddings))
	}
	for i, want := range []string{"dimensions", "encoding_format"} {
		r := c.Embeddings[i]
		if r.Field != want || r.RenameTo != "" {
			t.Errorf("embeddings[%d] = {%s→%s}, want plain %s strip", i, r.Field, r.RenameTo, want)
		}
		if r.Label == "" {
			t.Errorf("embeddings[%d] must carry the explanatory ada-002 label", i)
		}
		if !r.Applies("text-embedding-ada-002") || r.Applies("text-embedding-3-small") {
			t.Errorf("embeddings[%d] must gate on the ada-002 family", i)
		}
	}
}

// TestOpenAIContract_SamplingGates pins that every sampling strip in the
// table is gated by RejectsSamplingParams — the predicate carrying the
// gpt-5.4 carve-out — and the rename by NeedsMaxTokensRename. A rule
// re-wired to the wrong predicate re-opens the over-strip.
func TestOpenAIContract_SamplingGates(t *testing.T) {
	c := rewrites.OpenAIContract()
	// gpt-5.4 splits the two predicates; assert the split shows through
	// the table's gates. The forced-effort rule has its own gate
	// (gpt-5.6-only) and is asserted in TestOpenAIContract_Shape.
	for _, r := range c.Chat {
		if r.Field == "reasoning_effort" {
			continue
		}
		got := r.Applies("gpt-5.4")
		// What this test guards is the SAMPLING carve-out: gpt-5.4 accepts
		// temperature and top_p, so those two rules must not gate-match it.
		// Every other chat rule legitimately does — the max_tokens rename, and
		// the modalities strip, which was added after a replay showed gpt-5.4
		// answering 400 "Unknown parameter: 'modalities'" to real callers.
		//
		// Written as "not a sampling field" rather than "is max_tokens", which
		// is what it was: that phrasing made every future rule an assertion
		// failure regardless of whether the carve-out still held, so it would
		// have been relaxed under pressure rather than because the guarantee
		// changed.
		want := r.Field != "temperature" && r.Field != "top_p"
		if got != want {
			t.Errorf("chat rule %s: Applies(gpt-5.4) = %v, want %v", r.Field, got, want)
		}
		if !r.Applies("o3") {
			t.Errorf("chat rule %s must gate-match o3", r.Field)
		}
		if r.Applies("gpt-4o") {
			t.Errorf("chat rule %s must not gate-match gpt-4o", r.Field)
		}
	}
	for _, r := range c.Responses {
		if r.Applies("gpt-5.4") {
			t.Errorf("responses rule %s must not gate-match gpt-5.4 (probed 200)", r.Field)
		}
	}
}

// TestRejectsModalities_TheAudioCarveOutIsSubstringNotPrefix.
//
// Measured on the live gateway: 17 models answered "Unknown parameter:
// 'modalities'", across gpt-5.x and the o-series. Audio models are excluded
// EXPLICITLY rather than left to the generation test — stripping the field
// there turns a working request into "This model requires that either input
// content or output modality contain audio".
//
// Substring, not prefix, is the load-bearing part. A prefix catches today's
// "gpt-audio-*" and misses a future "gpt-6-audio", which WOULD match the
// generation test and lose the one field it cannot run without.
func TestRejectsModalities_TheAudioCarveOutIsSubstringNotPrefix(t *testing.T) {
	for _, m := range []string{"gpt-5.4", "gpt-5.4-mini", "gpt-5.6-luna", "o1", "o3-mini", "o4-mini"} {
		if !rewrites.RejectsModalities(m) {
			t.Errorf("%s: modalities is forwarded to a model measured to answer "+
				"\"Unknown parameter: 'modalities'\"", m)
		}
	}
	for _, m := range []string{"gpt-audio-mini", "gpt-6-audio", "gpt-4o-audio-preview"} {
		if rewrites.RejectsModalities(m) {
			t.Errorf("%s: modalities is stripped from an audio model, which then answers "+
				"\"This model requires that either input content or output modality contain "+
				"audio\" — a working request turned into a 400 by us", m)
		}
	}
	for _, m := range []string{"gpt-4o", "gpt-4-turbo", "text-embedding-3-small"} {
		if rewrites.RejectsModalities(m) {
			t.Errorf("%s: a field is stripped from a model that was never measured to "+
				"reject it", m)
		}
	}
}
