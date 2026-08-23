package proxy

import (
	"fmt"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// modalityUnitReservation describes a modality endpoint whose billable INPUT
// unit is derivable from the request JSON alone, so the quota pre-check and the
// /v1/estimate preview can reserve the SAME unit the post-call stamp bills
// (estimator.Lookup(EndpointKind), stampModalityUnits). It covers the three
// modalities ServeProxy routes through the generic quota/estimate path whose
// unit is a countable field of the request body:
//
//   - rerank → search units, ceil(documents/100)  (Cohere: 1 query + ≤100 docs)
//   - image  → images, request.n                   (per-image pricing, e.g. dall-e)
//   - tts    → input characters, request.input     (matches the stamp exactly)
//
// stt and video are deliberately absent: they are served by dedicated multipart
// handlers (ServeSTT / ServeVideoSubmit), never this path, and their units —
// input-audio seconds / generated-video seconds — cannot be known pre-call
// without decoding media. Every modality here prices on the INPUT column
// (costForInputUnits), so the count is reserved as input and nothing as output.
type modalityUnitReservation struct {
	InputUnits   int64                   // reserved against the input price column
	Units        estimator.BillableUnits // same count, shaped for estimator.Lookup
	EndpointKind string                  // cost-formula key for estimator.Lookup
	Assumption   string                  // preview note surfaced by /v1/estimate
}

// modalityUnitsFromRequest maps a catalog Model.Type ("image" / "tts" / "rerank"
// — the Model row taxonomy, NOT the typology.EndpointKind one; they diverge for
// image, "image" vs "image_generation") to its derivable input-unit reservation.
// ok=false for token endpoints (chat/embedding/responses) and for stt/video,
// which fall back to the token path. Single source of truth so the pre-check and
// the estimate preview never disagree with each other or with the stamp.
func modalityUnitsFromRequest(modelType string, body []byte) (modalityUnitReservation, bool) {
	switch modelType {
	case "rerank":
		// Cohere counts 1 search unit per query with up to 100 documents; >100
		// docs are ceil(docs/100) units. Over-counting is the safe direction for
		// a cost cap; a floor of 1 keeps a zero-document request from pricing $0
		// and bypassing the cap.
		units := (int64(len(gjson.GetBytes(body, "documents").Array())) + 99) / 100
		if units < 1 {
			units = 1
		}
		return modalityUnitReservation{
			InputUnits:   units,
			Units:        estimator.BillableUnits{SearchUnits: int(units)},
			EndpointKind: string(typology.EndpointKindRerank),
			Assumption:   fmt.Sprintf("rerank is priced per search unit; estimated %d search unit(s) from the request documents", units),
		}, true
	case "image":
		// Priced per generated image; request.n (default 1) is the count the
		// response returns (data.#), which the post-call stamp bills. Reserving
		// n images instead of the prompt's bytes-as-tokens removes the order-of-
		// magnitude over-reservation that would wrongly 429 a per-image model
		// under a cost quota. Token-priced image models (gpt-image-1) under-
		// reserve here — the safe direction — and reconcile to actual post-call.
		n := gjson.GetBytes(body, "n").Int()
		if n < 1 {
			n = 1
		}
		return modalityUnitReservation{
			InputUnits:   n,
			Units:        estimator.BillableUnits{Images: int(n)},
			EndpointKind: string(typology.EndpointKindImageGeneration),
			Assumption:   fmt.Sprintf("image generation is priced per image; estimated %d image(s) from request.n", n),
		}, true
	case "tts":
		// Priced per input character; the stamp reads the SAME request.input, so
		// the pre-check reserves exactly what will be billed. A floor of 1 keeps
		// an empty input from pricing $0 and bypassing the cap.
		chars := int64(utf8.RuneCountInString(gjson.GetBytes(body, "input").String()))
		if chars < 1 {
			chars = 1
		}
		return modalityUnitReservation{
			InputUnits:   chars,
			Units:        estimator.BillableUnits{InputChars: int(chars)},
			EndpointKind: string(typology.EndpointKindTTS),
			Assumption:   fmt.Sprintf("text-to-speech is priced per input character; counted %d character(s) from request.input", chars),
		}, true
	}
	return modalityUnitReservation{}, false
}

// preCallBillableUnits estimates the quota pre-check's input/output units in the
// SAME unit the routed model's price columns are denominated in — the unified
// pricing semantic where inputPricePerMillion is "USD per 1M billable units" and
// the unit is the endpoint's own (tokens for chat/embedding, search units for
// rerank, images for image, characters for tts). The quota cost formula is
// units × price / 1M, so estimating the endpoint's real unit here makes the
// pre-check reconcile with the post-call stamp instead of pricing, say, a rerank
// request's thousands of document "tokens" against a per-search rate. Modality
// rates live on the INPUT column, so a modality endpoint reserves its units as
// input and nothing as output.
func preCallBillableUnits(modelType string, body []byte) (inputUnits, outputUnits int64) {
	if r, ok := modalityUnitsFromRequest(modelType, body); ok {
		return r.InputUnits, 0
	}
	maxTokens := gjson.GetBytes(body, "max_tokens").Int()
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	return estimateTokens(body), maxTokens
}

// proxy_routing_quota.go holds the quota cost-estimate and downgrade-budget
// helpers used by checkQuota (in proxy_routing.go). Behavior unchanged.

// estimateTokens approximates the input token count of a request body for the
// quota PRE-CHECK cost estimate only — which is explicitly an approximation,
// reconciled to the actual usage post-call (see quota-architecture.md §6), so
// precision is not required. This is the QUOTA-class estimate; a fit / context-
// window check must use inputstaging.EstimateTokensConservative instead (biased
// high so it never admits an overflowing model). Do not add a further
// char→token heuristic — reuse the class that matches intent. It uses bytes/3
// rather than utf8.RuneCount(body)/3:
// counting runes over the whole ~50 KB body was ~7% of request-path CPU, and the
// byte length needs no scan. bytes/3 equals the prior rune/3 for ASCII and
// slightly OVER-estimates multi-byte UTF-8 (CJK), which is the safe direction for
// a quota pre-check (fails closed marginally earlier rather than under-reserving).
func estimateTokens(body []byte) int64 {
	est := int64(len(body)) / 3
	if est < 1 {
		est = 1
	}
	return est
}

// quotaHasCostLimit reports whether any level in the decision carries an
// enforced cost limit (Engine.Check stamps HasLimit on every level that
// resolved a positive cost cap). Used by the unpriced-model guard
// to fail closed only when a cost quota actually applies.
func quotaHasCostLimit(decision *quota.Decision) bool {
	if decision == nil {
		return false
	}
	for _, lvl := range decision.Levels {
		if lvl.HasLimit {
			return true
		}
	}
	return false
}

// quotaDowngradeBudget returns, in USD, the remaining headroom under the
// tightest enforced cap in the decision — the maximum spend a downgraded
// model may incur while still satisfying EVERY level's cost cap. Levels
// without a limit are ignored; a level already at/over its cap contributes
// 0 (forcing selection of the cheapest available model). Returns 0 when no
// level carries a limit.
func quotaDowngradeBudget(decision *quota.Decision) float64 {
	if decision == nil {
		return 0
	}
	budgetCents := int64(-1)
	for _, lvl := range decision.Levels {
		if !lvl.HasLimit {
			continue
		}
		remaining := lvl.LimitCents - lvl.CurrentCents
		if remaining < 0 {
			remaining = 0
		}
		if budgetCents < 0 || remaining < budgetCents {
			budgetCents = remaining
		}
	}
	if budgetCents < 0 {
		budgetCents = 0
	}
	return float64(budgetCents) / 100
}
