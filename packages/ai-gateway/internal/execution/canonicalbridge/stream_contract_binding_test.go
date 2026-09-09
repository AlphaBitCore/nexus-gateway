// Package canonicalbridge — the streaming contract must bind the type that
// implements it.
//
// Named failure modes:
//   - the declared stream field list names a field provcore.Chunk cannot carry,
//     so every cross-format stream drops it while the contract says otherwise
//   - Chunk carries a wire-visible channel the list does not name, so the next
//     person implementing "to the contract" deletes it
package canonicalbridge

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// chunkCarrier maps each declared stream path to the Chunk field that carries
// it, or to the reason it is deliberately not carried.
//
// This exists because the previous gate (TestSubsetFields_CoversCanonicalContract)
// checked the list against ITSELF — it asserted that the list contains "id",
// "model" and "choices". The list could name `choices[].index` for a year while
// Chunk had no per-choice representation at all, and the suite stayed green.
// That is exactly what happened: index and refusal were declared, unimplemented,
// and silently dropped on every cross-format stream.
//
// Cross-format is not an edge case. Same-format non-enforcing lanes forward the
// upstream frame verbatim, but every cross-spec call — and every same-format
// call a hook modifies — is rebuilt from the canonical chunk, so a field Chunk
// cannot hold is a field that call loses.
var chunkCarrier = map[string]string{
	// Envelope identity is re-synthesized per stream by the encoder, which owns
	// the ids it emits; carrying the upstream's would produce a frame whose id
	// disagrees with the rest of the stream it was spliced into.
	"id":      "synthesized by the encoder",
	"object":  "synthesized by the encoder",
	"created": "synthesized by the encoder",
	"model":   "carried on the call target, not per chunk",

	"choices":         "every Chunk is one choice's delta",
	"choices[].index": "ChoiceIndex",
	"choices[].delta": "the Chunk itself",
	// The encoder emits role on the first chunk of each choice (roleSent), so it
	// is derived rather than carried. Deriving is better than carrying: a
	// transcoded stream must open each choice exactly once regardless of how the
	// upstream framed it.
	"choices[].delta.role":    "derived by the encoder (roleSent)",
	"choices[].delta.content": "Delta",
	"choices[].delta.refusal": "RefusalDelta",
	// ReasoningDelta is the flat channel; NexusThinking carries the same content
	// when a wire signs its thinking blocks (Anthropic) and must ride alongside
	// rather than instead — both encode to this path.
	"choices[].delta.reasoning_content": "ReasoningDelta",
	"choices[].delta.tool_calls":        "ToolCallDeltas",
	"choices[].finish_reason":           "FinishReason",
	"usage":                             "Usage",

	// Upstream facts with no canonical carrier. Unlike role they cannot be
	// derived — only the upstream knows them — so naming them here is an
	// admission, not a design. See TestDeclaredButUncarriedFieldsAreDeliberate.
	"service_tier":       "NOT CARRIED",
	"system_fingerprint": "NOT CARRIED",
}

// wireVisibleChunkFields are the Chunk fields that reach the client, each of
// which must appear in the declared contract. Transport mechanics (RawBytes,
// NativeEvent, Verbatim, Truncated) are excluded: they steer HOW a chunk is
// forwarded, not WHAT the client is told.
var wireVisibleChunkFields = map[string]string{
	"Delta":          "choices[].delta.content",
	"RefusalDelta":   "choices[].delta.refusal",
	"ReasoningDelta": "choices[].delta.reasoning_content",
	"ToolCallDeltas": "choices[].delta.tool_calls",
	"FinishReason":   "choices[].finish_reason",
	"ChoiceIndex":    "choices[].index",
	"Usage":          "usage",
	"NexusThinking":  "choices[].delta.reasoning_content",
}

// TestEveryDeclaredStreamFieldHasACarrier walks the contract and demands each
// entry be accounted for — either by a Chunk field or by an explicit statement
// that it is not carried.
func TestEveryDeclaredStreamFieldHasACarrier(t *testing.T) {
	_, _, stream := SubsetFields()
	if len(stream) == 0 {
		t.Fatal("SubsetFields returned no stream paths — the contract would be vacuous")
	}

	chunkFields := structFieldNames(provcore.Chunk{})
	for _, path := range stream {
		carrier, declared := chunkCarrier[path]
		if !declared {
			t.Errorf("the stream contract declares %q and nothing here says what carries it. "+
				"Either map it to a Chunk field, or record that it is not carried and why — "+
				"an unaccounted path is how choices[].index stayed declared and unimplemented.",
				path)
			continue
		}
		if carrier == "NOT CARRIED" || strings.Contains(carrier, " ") {
			continue // an explicit non-carrier, or a prose explanation
		}
		if !chunkFields[carrier] {
			t.Errorf("the stream contract declares %q, carried by Chunk.%s — and Chunk has no "+
				"such field. Every cross-format stream drops it while the contract claims "+
				"otherwise.", path, carrier)
		}
	}
}

// TestEveryWireVisibleChunkFieldIsDeclared is the other direction. A channel
// Chunk carries that the contract does not name is a channel the next person
// implementing "to the contract" will delete — and reasoning was in exactly
// that position: load-bearing in Chunk, absent from the list.
func TestEveryWireVisibleChunkFieldIsDeclared(t *testing.T) {
	_, _, stream := SubsetFields()
	declared := make(map[string]bool, len(stream))
	for _, p := range stream {
		declared[p] = true
	}

	chunkFields := structFieldNames(provcore.Chunk{})
	for field, path := range wireVisibleChunkFields {
		if !chunkFields[field] {
			t.Errorf("wireVisibleChunkFields names Chunk.%s, which no longer exists — this map "+
				"has to track the type it describes or it stops testing anything", field)
			continue
		}
		if !declared[path] {
			t.Errorf("Chunk.%s reaches the client as %q, which the stream contract does not "+
				"declare. An undeclared channel is one a later 'implement the contract' pass "+
				"removes.", field, path)
		}
	}
}

// TestDeclaredButUncarriedFieldsAreDeliberate keeps the admissions honest: the
// list of things the canonical stream cannot carry is small, named, and does
// not grow quietly.
func TestDeclaredButUncarriedFieldsAreDeliberate(t *testing.T) {
	// Derived from production on BOTH axes, which the first version was not: it
	// walked the test-local carrier map and compared the result to a test-local
	// slice, so no production value was read and the only change it could
	// detect was an edit to its own file.
	//
	// The declared set comes from SubsetFields(), so a newly declared channel
	// nobody classified fails here. Whether a channel is carried comes from
	// reflection on provcore.Chunk, so implementing a carrier shrinks the set
	// and the admission list has to say so.
	_, _, stream := SubsetFields()
	chunkFields := structFieldNames(provcore.Chunk{})

	var uncarried []string
	for _, path := range stream {
		carrier, classified := chunkCarrier[path]
		if !classified {
			t.Errorf("the stream contract declares %q and nothing classifies it as carried or "+
				"not; an unclassified channel is one this admission list cannot speak for", path)
			continue
		}
		// The NOT CARRIED check comes FIRST, and the order is load-bearing: the
		// sentinel contains a space, so a prose-skip in front of it swallows
		// every admission and the set comes back empty — which reads as "nothing
		// is uncarried" rather than "the filter ate them".
		if carrier == "NOT CARRIED" {
			uncarried = append(uncarried, path)
			continue
		}
		if strings.Contains(carrier, " ") {
			continue // a prose explanation of how it is derived, not a carrier
		}
		if !chunkFields[carrier] {
			uncarried = append(uncarried, path)
		}
	}
	sort.Strings(uncarried)

	want := []string{"service_tier", "system_fingerprint"}
	if strings.Join(uncarried, ",") != strings.Join(want, ",") {
		t.Errorf("the set of declared-but-uncarried stream fields is %v, expected %v.\n"+
			"Growing it means a cross-format stream lost something new; shrinking it means "+
			"something was implemented and this list should say so.", uncarried, want)
	}
}

// structFieldNames reports the exported field names of a struct. Reflection
// rather than a hand-kept list: a hand-kept list of "what Chunk has" would be a
// third thing to keep in sync, and the drift this file exists to catch is
// exactly a list falling behind a type.
func structFieldNames(v any) map[string]bool {
	t := reflect.TypeOf(v)
	out := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		out[t.Field(i).Name] = true
	}
	return out
}
