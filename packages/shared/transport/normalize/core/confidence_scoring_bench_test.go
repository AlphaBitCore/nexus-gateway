package core

import (
	"fmt"
	"strings"
	"testing"
)

// These three arms exist to close a candidate, not to open one. Read together
// they say the scorer has nothing worth optimising, and each arm rules out one
// of the three ideas that looked promising from the code:
//
//	Whole      ~3,070 ns   8,549 B   34 allocs
//	ParseOnly  ~2,840 ns   8,549 B   34 allocs
//	SetsOnly       ~71 ns       0 B    0 allocs
//
//  1. The scorer is its parse. Everything downstream of topLevelKeys is ~230 ns
//     and allocates nothing, so there is no non-parse target here.
//  2. Rebuilding the two sets per call is free — the slices are small and static
//     and the maps do not escape. The "cache them on the FieldSpec" idea buys
//     zero.
//  3. The parse itself is already the cheapest available: BenchmarkTopLevelKeys
//     in this package measured a hand-written token walk at 13x the allocations
//     and a validate-then-decode at 19x.
//
// The one idea left is to have each codec hand the scorer the keys it already
// saw, removing the second pass entirely. That does not work either: the score
// weights UNKNOWN keys, and a codec decoding into its own typed struct discards
// exactly those. The scorer needs what the codec's decode throws away, which is
// why it reads the body a second time.
//
// A 49 KB body is used by the older benchmark; this one uses ~2 KB because the
// parse scales with length and production bodies on this path are nearer 2 KB.
// The conclusion is the same at both sizes.

func realisticChatBody() []byte {
	var b strings.Builder
	b.WriteString(`{"id":"chatcmpl-BxKq3","object":"chat.completion","created":1756000000,`)
	b.WriteString(`"model":"gpt-4o-mini","system_fingerprint":"fp_0ba0d124f1","choices":[{"index":0,`)
	b.WriteString(`"message":{"role":"assistant","content":"`)
	for i := range 40 {
		fmt.Fprintf(&b, "sentence %d of a perfectly ordinary assistant reply. ", i)
	}
	b.WriteString(`"},"logprobs":null,"finish_reason":"stop"}],`)
	b.WriteString(`"usage":{"prompt_tokens":112,"completion_tokens":486,"total_tokens":598}}`)
	return []byte(b.String())
}

var realisticChatSpec = FieldSpec{
	Required: []string{"model", "choices", "usage"},
	Optional: []string{"id", "object", "created", "system_fingerprint", "service_tier"},
}

// BenchmarkScoreTier1_Whole is what a codec actually calls, at a realistic body
// size. Sixteen Tier-1 codecs reach it, two to four times per gateway request.
func BenchmarkScoreTier1_Whole(b *testing.B) {
	raw := realisticChatBody()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = scoreTier1Confidence(raw, realisticChatSpec)
	}
}

// BenchmarkScoreTier1_ParseOnly is the half the earlier benchmark already
// proved is not cheaply avoidable. Subtracting it from the whole leaves the
// half that is.
func BenchmarkScoreTier1_ParseOnly(b *testing.B) {
	raw := realisticChatBody()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = topLevelKeys(raw)
	}
}

// BenchmarkScoreTier1_SetsOnly is the two sets rebuilt per call from static
// spec slices. Nothing about them depends on the request.
func BenchmarkScoreTier1_SetsOnly(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = stringSet(realisticChatSpec.Required)
		_ = stringSet(realisticChatSpec.Optional)
	}
}
