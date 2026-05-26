package core

import (
	"bytes"
	"encoding/json"
)

// FieldSpec declares the wire-level top-level keys a Tier-1 normalizer
// knows how to handle for one (protocol, direction) pair. The split
// between Required and Optional drives the weighted confidence score:
// missing a Required key heavily penalises confidence; missing an
// Optional key mildly penalises; observing a key in neither set (a
// likely spec drift) applies a small bounded penalty.
//
// Each normalizer declares one FieldSpec per direction as a package
// variable, typically alongside the response/request struct definitions.
type FieldSpec struct {
	// Required lists keys whose presence is structurally necessary for
	// a complete parse. Examples:
	//   - openai-chat response: model, choices, usage
	//   - anthropic-messages response: model, content, usage, stop_reason
	Required []string
	// Optional lists keys whose presence improves the parse but whose
	// absence is cosmetic. Examples:
	//   - openai-chat response: id, object, created, system_fingerprint
	Optional []string
}

// scoreTier1Confidence computes a confidence score in [0.40, 1.00] for
// a successfully-parsed Tier-1 normalizer payload, by comparing the
// top-level keys actually present in raw against the normalizer's
// declared FieldSpec.
//
//	score = 0.50 (baseline shape match)
//	      + 0.40 × (required_recognised / required_total)
//	      + min(0.10, 0.025 × optional_recognised)
//	      − 0.10 × (unknown_observed / max(1, total_observed))
//
// The Coordinator threshold (Registry.threshold, default 0.70) decides
// whether to keep Tier-1 or fall through to Tier-2 pattern probes.
// Numerical anchor points (3 required declared, 4+ optional observed):
//
//	full parse, no unknowns:        0.50 + 0.400 + 0.100 − 0.00 = 1.00
//	one required missing:           0.50 + 0.267 + 0.100 − 0.00 = 0.867
//	two required missing:           0.50 + 0.133 + 0.100 − 0.00 = 0.733
//	all required missing:           0.50 + 0.000 + 0.100 − 0.00 = 0.600 (sub-threshold)
//	full + 3 unknown of 11 observed: 1.00 − 0.0273              = 0.973
//	all-unknown body:               0.50 + 0 + 0                = 0.40 (floor)
//
// Optional bonus uses a per-field 0.025 contribution capped at 0.10 —
// NOT a ratio across the declared optional set. The ratio formulation
// (older draft) penalised normalizers with longer optional declarations
// even though longer-list normalizers were just being more thorough
// about declaring known fields. With the fixed per-field bonus, two
// healthy parses with the same observed-optional count score
// identically regardless of how many additional optional fields the
// normalizer's spec happens to declare. 4 observed optional fields
// saturate the bonus, which is the realistic "well-formed response"
// threshold across our normalizers.
//
// Why these weights, not equal thirds? Required absence is a strong
// negative signal — a response with no usage may be a truncated
// stream-final frame, a refund/abstain response, or a genuine API
// change worth investigating. Optional absence is more often "this
// provider just doesn't emit X" (Cohere has no system_fingerprint,
// Replicate has no `id` at the response root). Weighting prevents the
// optional floor from dragging down a clean parse while letting
// required absence dominate when it actually matters.
//
// Unknown-field penalty is bounded at −0.10 even when 100% of observed
// keys are unknown — providers regularly emit harmless additions and
// we don't want one new vendor extension flipping the row to Tier-2.
//
// Empty observed set (zero-byte body, unparseable JSON, SSE we can't
// peel) returns 0.90 — the normalizer DID return nil err, so we don't
// pessimise to floor, but we can't measure ratios so we don't claim
// 1.0 either. Matches the pre-FieldSpec rubric's baseline so legacy
// callers that haven't been migrated still get a sensible score.
// ScoreTier1Confidence computes the confidence score for a successfully-parsed
// Tier-1 normalizer payload. Exported so the codecs sub-package can call it
// without duplicating the scoring logic.
func ScoreTier1Confidence(raw []byte, spec FieldSpec) float64 {
	return scoreTier1Confidence(raw, spec)
}

func scoreTier1Confidence(raw []byte, spec FieldSpec) float64 {
	observed := topLevelKeys(raw)
	if len(observed) == 0 {
		return 0.90
	}

	required := stringSet(spec.Required)
	optional := stringSet(spec.Optional)
	var seenReq, seenOpt, unknown int
	for k := range observed {
		switch {
		case required[k]:
			seenReq++
		case optional[k]:
			seenOpt++
		default:
			unknown++
		}
	}

	score := 0.50
	if n := len(spec.Required); n > 0 {
		score += 0.40 * float64(seenReq) / float64(n)
	} else {
		score += 0.40
	}
	optBonus := 0.025 * float64(seenOpt)
	if len(spec.Optional) == 0 {
		optBonus = 0.10 // no optional declared → don't penalise the parse
	}
	if optBonus > 0.10 {
		optBonus = 0.10
	}
	score += optBonus
	score -= 0.10 * float64(unknown) / float64(len(observed))

	if score < 0.40 {
		score = 0.40
	}
	if score > 1.00 {
		score = 1.00
	}
	return score
}

// topLevelKeys returns the set of top-level keys in raw's outermost
// JSON object. Handles three wire shapes:
//
//  1. Plain JSON object → enumerate keys directly.
//  2. SSE event stream (starts with "data:" or "event:") → enumerate
//     the FIRST `data:` chunk's JSON object keys. Representative
//     enough that the score reflects whether the stream shape matched
//     expectations.
//  3. JSON array root, NDJSON, malformed, non-JSON → returns nil.
//     scoreTier1Confidence treats nil as the 0.90 baseline.
func topLevelKeys(raw []byte) map[string]struct{} {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
		chunk := firstSSEDataChunk(trimmed)
		if len(chunk) == 0 {
			return nil
		}
		trimmed = chunk
	}
	if trimmed[0] != '{' {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil
	}
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// firstSSEDataChunk extracts the JSON payload from the first `data:`
// line of an SSE stream. Returns nil when no usable payload is found
// (e.g., the stream is `[DONE]`-only or only `event:` framing).
func firstSSEDataChunk(raw []byte) []byte {
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		return payload
	}
	return nil
}

func stringSet(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}
