package codec

import (
	"fmt"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RewriteNative applies the Anthropic same-spec differential to a native
// /v1/messages body: the resolved-model stamp plus the SAME sampling-param and
// max_tokens coercions EncodeRequest applies on the cross-format leg, so the
// two entry points behave identically (the §2 two-doors property).
//
// D3 (owner-approved 2026-07-16, design v3 §"decisions"): migrating Anthropic
// turns a native temperature/top_p body sent to a rejects-sampling family — and
// an over-ceiling (or absent, which Anthropic 400s as "Field required")
// max_tokens — from a deterministic upstream 400 into a gateway coerce made
// visible via the x-nexus-coerced response header. The 400 path has no
// beneficiary: the request was failing regardless, and the coerced success is
// observable to the caller.
//
// This differential is limited to the Rule-7-cited sampling/ceiling rules.
// Native features (thinking blocks, tool_use, image parts), error shapes, and
// response format ride through VERBATIM — the differential is not a license to
// rewrite native requests beyond the evidence-cited quirks.
//
// Dispatch invokes this only with a resolved (non-empty) ProviderModelID — the
// prepareNative degenerate-target guard returns the body verbatim when no model
// resolved — so no empty-model fallback is needed here.
func (Codec) RewriteNative(_ typology.WireShape, nativeBody []byte, target provcore.CallTarget, _ bool) (provcore.EncodeResult, error) {
	out, err := provcore.SurgicalModelStamp(nativeBody, target.ProviderModelID)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	out, rewrites := applyNativeSamplingPolicy(out, target.ProviderModelID, target.MaxOutputTokens)
	return provcore.EncodeResult{Body: out, ContentType: "application/json", Rewrites: rewrites}, nil
}

// applyNativeSamplingPolicy edits a native Anthropic body in place to match the
// per-model max_tokens + sampling policy EncodeRequest applies while rebuilding
// (codec.go). It shares the exact same predicate functions
// (anthropicModelRejectsSamplingParams / anthropicModelRejectsTempTopPTogether /
// clampMaxTokens) so the two doors cannot drift, applies the max_tokens rule
// FIRST and the sampling strips after (matching EncodeRequest's field order so
// the emitted rewrites — and thus the x-nexus-coerced header string — are byte-
// identical between doors), and emits a rewrite for every mutation. Each root
// field is probed once (gjson has no cross-Get cursor). Fields not named here
// are untouched; a conformant body forwards as the same slice.
func applyNativeSamplingPolicy(body []byte, model string, limit int) ([]byte, []string) {
	root := gjson.ParseBytes(body)
	temp := root.Get("temperature")
	topP := root.Get("top_p")
	topK := root.Get("top_k")
	maxTok := root.Get("max_tokens")
	maxCompletion := root.Get("max_completion_tokens")

	rejectsSampling := anthropicModelRejectsSamplingParams(model)
	coexistsTopPWithTemp := !rejectsSampling && anthropicModelRejectsTempTopPTogether(model) &&
		temp.Exists() && topP.Exists()

	var rewrites []string
	out := body

	// max_tokens first, matching EncodeRequest's field + rewrite order.
	// max_completion_tokens (the OpenAI-only successor) takes precedence when
	// present, as EncodeRequest resolves it. A genuine native Anthropic body
	// never carries it — the Anthropic wire has only max_tokens — but honouring
	// it into max_tokens (and dropping the foreign key) keeps the two doors at
	// parity on any body and avoids forwarding a key Anthropic would 400 on.
	switch {
	case maxCompletion.Exists():
		clamped := clampMaxTokens(maxCompletion.Int(), limit, &rewrites)
		out, _ = sjson.SetBytes(out, "max_tokens", clamped)
		out, _ = sjson.DeleteBytes(out, "max_completion_tokens")
	case maxTok.Exists():
		requested := maxTok.Int()
		if clamped := clampMaxTokens(requested, limit, &rewrites); clamped != requested {
			out, _ = sjson.SetBytes(out, "max_tokens", clamped)
		}
	default:
		filled := limit
		if filled <= 0 {
			filled = anthropicFallbackMaxOutput
		}
		out, _ = sjson.SetBytes(out, "max_tokens", filled)
		rewrites = append(rewrites, fmt.Sprintf("max_tokens→%d_model_default", filled))
	}

	if temp.Exists() && rejectsSampling {
		out, _ = sjson.DeleteBytes(out, "temperature")
		rewrites = append(rewrites, "temperature→removed")
	}
	if topP.Exists() {
		switch {
		case rejectsSampling:
			out, _ = sjson.DeleteBytes(out, "top_p")
			rewrites = append(rewrites, "top_p→removed")
		case coexistsTopPWithTemp:
			out, _ = sjson.DeleteBytes(out, "top_p")
			rewrites = append(rewrites, "top_p→removed_with_temperature_present")
		}
	}
	if topK.Exists() && rejectsSampling {
		out, _ = sjson.DeleteBytes(out, "top_k")
		rewrites = append(rewrites, "top_k→removed")
	}

	return out, rewrites
}
