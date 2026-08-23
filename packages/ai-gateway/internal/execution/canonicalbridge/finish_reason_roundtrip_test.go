// finish_reason_roundtrip_test.go asserts the property the per-value tests
// cannot: that the Anthropic stop_reason mappers are actually inverse, and
// that the three independent implementations of the reverse direction agree.
//
// Per-value pinning only proves each mapper does what it was written to do.
// It is silent on the question that matters to a caller — does a value that
// went out come back as itself — and it was silent while content_filter
// round-tripped through "stop_sequence", telling an Anthropic caller their
// own custom stop string had fired when the answer had actually been
// filtered.
package canonicalbridge

import (
	"testing"

	anthropiccodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/codec"
	anthropicingress "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	geminicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
)

// Every Anthropic stop_reason that has a canonical counterpart must survive
// the trip out to canonical and back. The values without one are listed
// separately below, with the reason each is allowed to lose fidelity.
func TestAnthropicStopReason_RoundTripsThroughCanonical(t *testing.T) {
	for _, stopReason := range []string{
		"end_turn",
		"max_tokens",
		"tool_use",
		"refusal",
	} {
		canonical := anthropiccodec.MapStopReason(stopReason)
		if canonical == stopReason {
			t.Errorf("%q was not translated at all — it should map into the canonical vocabulary", stopReason)
			continue
		}
		if back := canonicalFinishToAnthropicStop(canonical); back != stopReason {
			t.Errorf("%q → canonical %q → %q; a stop_reason must come back as itself",
				stopReason, canonical, back)
		}
	}
}

// stop_sequence and model_context_window_exceeded are the two values that
// legitimately do not round-trip, and the reasons are different. Pinned so a
// future change has to state which case it is rather than quietly widening
// the set of values that get rewritten.
func TestAnthropicStopReason_DocumentedNonRoundTrips(t *testing.T) {
	// OpenAI's "stop" genuinely covers both end_turn and stop_sequence, so
	// this is a projection onto a smaller vocabulary, not a wrong value. The
	// canonical carries no slot for WHICH stop sequence matched, so the
	// return leg can only pick the general case.
	if got := anthropiccodec.MapStopReason("stop_sequence"); got != "stop" {
		t.Errorf("stop_sequence → %q, want stop", got)
	}
	if got := canonicalFinishToAnthropicStop("stop"); got != "end_turn" {
		t.Errorf("stop → %q, want end_turn (the general case, since the matched sequence is not carried)", got)
	}

	// model_context_window_exceeded and max_tokens both mean "ran out of
	// room", which is what OpenAI's "length" says. The return leg picks
	// max_tokens because it is the case a caller can act on.
	if got := anthropiccodec.MapStopReason("model_context_window_exceeded"); got != "length" {
		t.Errorf("model_context_window_exceeded → %q, want length", got)
	}
}

// pause_turn must survive untranslated. It means the turn is resumable by
// resubmitting the response as-is; every canonical finish_reason claims the
// answer is over, so any mapping at all would be a false statement.
func TestAnthropicStopReason_PauseTurnIsNotFlattened(t *testing.T) {
	if got := anthropiccodec.MapStopReason("pause_turn"); got != "pause_turn" {
		t.Errorf("pause_turn → %q; folding a resumable turn into a terminal finish_reason tells the caller the answer is finished", got)
	}
}

// The reverse direction has three independent implementations — the stream
// encoder here, the hub-ingress translator, and (in lockstep, verified by its
// own parity test) the anthropic stream decoder. Two of them disagreeing is
// how content_filter came to mean stop_sequence on one path and not another.
func TestAnthropicReverseMappers_Agree(t *testing.T) {
	for _, canonical := range []string{
		"stop", "length", "tool_calls", "content_filter", "",
		"some_unshipped_value",
	} {
		bridge := canonicalFinishToAnthropicStop(canonical)
		ingress := anthropicingress.MapOpenAIFinishToStopReason(canonical)
		if bridge != ingress {
			t.Errorf("canonical %q: stream encoder says %q, hub ingress says %q — the two reverse mappers must not diverge",
				canonical, bridge, ingress)
		}
	}
}

// Gemini gets the same two properties Anthropic does. It is the other wire
// whose forward mapper has a second copy (gemini/stream mirrors gemini/codec,
// kept in step by their own parity test) and whose reverse mapper lives here —
// and reverse mappers are where the wrong-value defects were: content_filter
// became Anthropic's stop_sequence, and MALFORMED_FUNCTION_CALL became
// tool_calls. Nothing was asserting that this one is an inverse at all.
func TestGeminiFinishReason_RoundTripsThroughCanonical(t *testing.T) {
	for _, geminiReason := range []string{
		"STOP",
		"MAX_TOKENS",
	} {
		canonical := geminicodec.MapFinishReason(geminiReason)
		if canonical == geminiReason {
			t.Errorf("%q was not translated at all", geminiReason)
			continue
		}
		if back := canonicalFinishToGemini(canonical); back != geminiReason {
			t.Errorf("%q → canonical %q → %q; a finishReason with a canonical counterpart must come back as itself",
				geminiReason, canonical, back)
		}
	}
}

// The values that legitimately do not round-trip, each with the reason it is
// allowed to lose fidelity. Pinned so widening this set requires saying which
// case it is rather than quietly adding to it.
func TestGeminiFinishReason_DocumentedNonRoundTrips(t *testing.T) {
	// Eight distinct block reasons collapse onto one canonical value. The
	// return leg can only pick the general one; the canonical carries no slot
	// for WHICH classifier fired.
	for _, blocked := range []string{
		"SAFETY", "RECITATION", "LANGUAGE", "PROHIBITED_CONTENT",
		"SPII", "BLOCKLIST", "IMAGE_SAFETY", "MODEL_ARMOR",
	} {
		if got := geminicodec.MapFinishReason(blocked); got != "content_filter" {
			t.Errorf("%s → %q, want content_filter", blocked, got)
		}
	}
	if got := canonicalFinishToGemini("content_filter"); got != "SAFETY" {
		t.Errorf("content_filter → %q, want SAFETY (the general case)", got)
	}

	// MALFORMED_FUNCTION_CALL and UNEXPECTED_TOOL_CALL pass through raw rather
	// than claiming tool_calls, so they have no canonical counterpart to
	// return from. Asserting it here keeps the earlier fix from being undone
	// by someone completing the switch by analogy.
	for _, noUsableCall := range []string{"MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL"} {
		if got := geminicodec.MapFinishReason(noUsableCall); got != noUsableCall {
			t.Errorf("%s → %q; a turn that produced no usable tool call must not claim one", noUsableCall, got)
		}
	}
}
