// estimate.go — POST /v1/estimate compare endpoint.
//
// Pre-flight cost estimate for one request body against one OR many
// candidate targets. Uses VK authentication (same as /v1/*). Per-target
// dispatch is parallel; partial failures (one bad target) are reported
// per-target so a single failure doesn't break the whole response.
//
// Minimal v1 surface: accept original ingress request body + optional
// compareTargets array; dispatch the estimator for each target; return
// per-target estimate + a top-level summary identifying the cheapest
// target (by Cost.Expected.Total).

package proxy

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
)

// EstimateRequest is the POST /v1/estimate request body.
type EstimateRequest struct {
	Request        json.RawMessage         `json:"request"`
	CompareTargets []EstimateCompareTarget `json:"compareTargets"`
	Options        EstimateRequestOptions  `json:"options,omitempty"`
}

// EstimateCompareTarget identifies one (Provider, Model) candidate.
// ModelID accepts UUID or human-friendly code (Model.code).
type EstimateCompareTarget struct {
	ProviderID      string  `json:"providerId"`
	ModelID         string  `json:"modelId"`
	ReasoningEffort *string `json:"reasoningEffort,omitempty"`
}

// EstimateRequestOptions controls estimator behavior.
type EstimateRequestOptions struct {
	IngressFormat *string `json:"ingressFormat,omitempty"`
}

// EstimateResponse is the response shape.
type EstimateResponse struct {
	Targets []EstimatePerTarget    `json:"targets"`
	Summary EstimateCompareSummary `json:"summary"`
}

// EstimatePerTarget is one row in the response.
type EstimatePerTarget struct {
	ProviderID   string                        `json:"providerId"`
	ProviderName string                        `json:"providerName,omitempty"`
	ModelID      *string                       `json:"modelId"`
	ModelCode    string                        `json:"modelCode,omitempty"`
	Tokens       *estimator.TokenBreakdown     `json:"tokens,omitempty"`
	Cost         *estimator.CostBreakdown      `json:"cost,omitempty"`
	Reasoning    *estimator.ReasoningBreakdown `json:"reasoning,omitempty"`
	Assumptions  []string                      `json:"assumptions,omitempty"`
	Error        *EstimateTargetError          `json:"error,omitempty"`
}

// EstimateTargetError carries a structured per-target failure.
type EstimateTargetError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EstimateCompareSummary derives top-line numbers across the per-target
// results — useful for a "cheapest" badge on UI.
type EstimateCompareSummary struct {
	CheapestExpectedTarget        *string  `json:"cheapestExpectedTarget,omitempty"`
	CheapestExpectedTotalUsd      *float64 `json:"cheapestExpectedTotalUsd,omitempty"`
	MostExpensiveExpectedTotalUsd *float64 `json:"mostExpensiveExpectedTotalUsd,omitempty"`
	ErrorsCount                   int      `json:"errorsCount"`
	SuccessCount                  int      `json:"successCount"`
}

const estimateConcurrency = 8

// validReasoningEfforts enumerates the per-target reasoningEffort
// override values. Integer values (Anthropic / Gemini budget_tokens)
// are accepted as numeric strings or as digits.
var validReasoningEfforts = map[string]bool{
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
}

func isValidReasoningEffort(v string) bool {
	if v == "" {
		return true
	}
	if validReasoningEfforts[strings.ToLower(v)] {
		return true
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100000 {
		return true
	}
	return false
}

// ServeEstimate handles POST /v1/estimate.
func (h *Handler) ServeEstimate(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeEstimateError(w, http.StatusMethodNotAllowed, "ESTIMATE_METHOD_NOT_ALLOWED", "POST only")
		return
	}

	// VK auth — same surface as the proxy /v1/* endpoints.
	vkMeta, err := h.authenticate(r)
	if err != nil {
		writeEstimateError(w, http.StatusUnauthorized, "ESTIMATE_UNAUTHORIZED", err.Error())
		return
	}

	// Separate per-VK compareEndpointRateLimit bucket.
	if err := h.checkCompareRateLimit(w, vkMeta); err != nil {
		writeEstimateError(w, http.StatusTooManyRequests, "ESTIMATE_COMPARE_RATE_LIMITED", err.Error())
		return
	}

	var req EstimateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEstimateError(w, http.StatusBadRequest, "ESTIMATE_INVALID_JSON", err.Error())
		return
	}
	if len(req.CompareTargets) == 0 {
		writeEstimateError(w, http.StatusBadRequest, "ESTIMATE_NO_TARGETS",
			"compareTargets array must contain at least 1 target")
		return
	}
	if len(req.CompareTargets) > 10 {
		writeEstimateError(w, http.StatusBadRequest, "ESTIMATE_TOO_MANY_TARGETS",
			"compareTargets exceeds maximum of 10 entries per request")
		return
	}
	if len(req.Request) == 0 {
		writeEstimateError(w, http.StatusBadRequest, "ESTIMATE_NO_REQUEST",
			"request body is required")
		return
	}

	// options.ingressFormat reached a Prometheus label value straight from the
	// body, so any authenticated caller could mint unbounded label cardinality
	// on two counters. Refusing an unrecognised value bounds that, and stops
	// the endpoint accepting a word it cannot act on.
	if f := req.Options.IngressFormat; f != nil && *f != "" && !provcore.Format(*f).Valid() {
		writeEstimateError(w, http.StatusBadRequest, "ESTIMATE_INVALID_INGRESS_FORMAT",
			fmt.Sprintf("options.ingressFormat=%q is not a wire format this gateway speaks", *f))
		return
	}

	// Validate reasoningEffort overrides at the request level so a
	// per-target invalid value fails fast instead of silently degrading
	// to the default.
	for i, t := range req.CompareTargets {
		if t.ReasoningEffort == nil {
			continue
		}
		if !isValidReasoningEffort(*t.ReasoningEffort) {
			writeEstimateError(w, http.StatusBadRequest, "ESTIMATE_INVALID_REASONING_EFFORT",
				fmt.Sprintf("compareTargets[%d].reasoningEffort=%q must be one of {minimal, low, medium, high} or a positive integer budget", i, *t.ReasoningEffort))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	results := make([]EstimatePerTarget, len(req.CompareTargets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, estimateConcurrency)
	for i, target := range req.CompareTargets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, target EstimateCompareTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = h.runEstimateOnce(ctx, req.Request, target, vkMeta)
		}(i, target)
	}
	wg.Wait()

	// Top-level telemetry — 1 request, N targets, full duration.
	if h.deps != nil && h.deps.Metrics != nil {
		ingress := "openai" // request body shape detection is a future enhancement
		if req.Options.IngressFormat != nil && *req.Options.IngressFormat != "" {
			ingress = *req.Options.IngressFormat
		}
		h.deps.Metrics.RecordEstimateCompare(ingress, len(req.CompareTargets), time.Since(startedAt))
	}

	resp := EstimateResponse{
		Targets: results,
		Summary: buildEstimateSummary(results),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) runEstimateOnce(ctx context.Context, body []byte, target EstimateCompareTarget, vkMeta *vkauth.VKMeta) EstimatePerTarget {
	out := EstimatePerTarget{
		ProviderID: target.ProviderID,
		ModelCode:  target.ModelID,
	}

	m, ok := h.resolveTargetModel(ctx, target)
	if !ok {
		out.Error = &EstimateTargetError{
			Code:    "ESTIMATE_TARGET_NOT_FOUND",
			Message: fmt.Sprintf("model %q under provider %q not found in catalog", target.ModelID, target.ProviderID),
		}
		return out
	}
	out.ModelID = &m.ID
	out.ModelCode = m.Code
	out.ProviderID = m.ProviderID
	out.ProviderName = m.ProviderName

	// Per-target VK allowedModels enforcement. Each violated target gets
	// a per-target error so a partially-restricted VK still gets useful
	// per-target estimates for the accessible subset (vs failing the
	// whole compare with a top-level 403).
	if vkMeta != nil && len(vkMeta.AllowedModels) > 0 &&
		!routingcore.ModelMatchesAllowedRefs(m.ID, m.ProviderModelID, m.ProviderID, vkMeta.AllowedModels) {
		vkName := vkMeta.Name
		if vkName == "" {
			vkName = vkMeta.ID
		}
		out.Error = &EstimateTargetError{
			Code:    "vk_model_not_allowed",
			Message: fmt.Sprintf("VK %q allowedModels does not include %q (providerId=%s)", vkName, m.Code, m.ProviderID),
		}
		return out
	}

	prices := metrics.ModelPrices{
		InputUsdPerM:            m.InputPricePM,
		OutputUsdPerM:           m.OutputPricePM,
		CachedInputReadUsdPerM:  m.CachedInputReadPricePM,
		CachedInputWriteUsdPerM: m.CachedInputWritePricePM,
	}

	// Modality endpoints whose unit is derivable from the request (rerank →
	// search units, image → images, tts → input characters) are priced per that
	// unit, not per token. Tokenizing the request and multiplying by a per-image
	// or per-search rate wildly misstates the preview. Price via the SAME formula
	// (estimator.Lookup(EndpointKind)) the actual cost stamp uses so the estimate
	// reconciles with the bill. stt/video are absent from modalityUnitsFromRequest
	// — their units are not knowable pre-call — so they fall through to the token
	// path below.
	if r, ok := modalityUnitsFromRequest(m.Type, body); ok {
		cost := estimator.Lookup(r.EndpointKind)(r.Units, prices)
		out.Cost = &estimator.CostBreakdown{Currency: "USD", Low: cost, Expected: cost, High: cost}
		out.Assumptions = []string{r.Assumption}
		return out
	}

	maxOutput := 0
	if m.MaxOutputTokens != nil {
		maxOutput = *m.MaxOutputTokens
	}

	in := estimator.EstimateInput{
		CanonicalRequest: body,
		IngressFormat:    provcore.FormatOpenAI,
		ReasoningEffortOverride: func() string {
			if target.ReasoningEffort == nil {
				return ""
			}
			return *target.ReasoningEffort
		}(),
		Target: estimator.ResolvedTarget{
			ProviderID:  m.ProviderID,
			ModelID:     m.ID,
			ModelCode:   m.Code,
			AdapterType: m.ProviderAdapterType,
			MaxOutput:   maxOutput,
		},
		Prices: prices,
	}

	estStart := time.Now()
	res, err := estimator.Estimate(ctx, in)
	if h.deps != nil && h.deps.Metrics != nil {
		// Per-target telemetry — counts every dispatch, succeeded or
		// failed. The compare endpoint shares the same counter so dashboards
		// have one fan-in.
		h.deps.Metrics.RecordEstimate("openai", m.Code, m.ProviderName, time.Since(estStart))
	}
	if err != nil {
		out.Error = &EstimateTargetError{
			Code:    "ESTIMATE_FAILED",
			Message: err.Error(),
		}
		return out
	}

	out.Tokens = &res.Tokens
	out.Cost = &res.Cost
	out.Reasoning = &res.Reasoning
	out.Assumptions = res.Assumptions
	return out
}

// resolveTargetModel finds the (providerId, modelId) row the caller named.
//
// providerId used to be echoed back and nothing else: the lookup was by code
// alone, so two providers serving one code both resolved to whichever row the
// catalog returned first, and a compare across two providers of the same model
// — the endpoint's whole purpose — priced both at one provider's rates. It is
// consulted first now, and the by-code lookups stay as the fallback for a
// caller that names only the model.
func (h *Handler) resolveTargetModel(ctx context.Context, target EstimateCompareTarget) (store.Model, bool) {
	if h.deps == nil || h.deps.Models == nil {
		return store.Model{}, false
	}
	if target.ProviderID != "" {
		if rows, err := h.deps.Models.ListEnabledModels(ctx); err == nil {
			for _, m := range rows {
				if m.ProviderID != target.ProviderID {
					continue
				}
				if m.Code == target.ModelID || m.ID == target.ModelID {
					return m, true
				}
			}
		}
	}
	if m, err := h.deps.Models.GetModelByCode(ctx, target.ModelID); err == nil && m != nil {
		return *m, true
	}
	if m, err := h.deps.Models.GetModel(ctx, target.ModelID); err == nil && m != nil {
		return *m, true
	}
	return store.Model{}, false
}

func buildEstimateSummary(targets []EstimatePerTarget) EstimateCompareSummary {
	s := EstimateCompareSummary{}
	var cheapest, mostExp *float64
	var cheapestName string
	for _, t := range targets {
		if t.Error != nil {
			s.ErrorsCount++
			continue
		}
		s.SuccessCount++
		if t.Cost == nil {
			continue
		}
		total := t.Cost.Expected.Total
		if cheapest == nil || total < *cheapest {
			c := total
			cheapest = &c
			cheapestName = t.ModelCode
		}
		if mostExp == nil || total > *mostExp {
			m := total
			mostExp = &m
		}
	}
	if cheapest != nil {
		name := cheapestName
		s.CheapestExpectedTarget = &name
		s.CheapestExpectedTotalUsd = cheapest
		s.MostExpensiveExpectedTotalUsd = mostExp
	}
	return s
}

// writeEstimateError answers through the single gateway-error envelope.
//
// This route had its own shape: a lower_snake code and no type at all, on the
// reasoning that the alternative at the time put a numeric status in
// error.code. Wanting a stable string slug was right; owning a private
// envelope to get one was not, and it left estimate as the only route whose
// errors carry no type. The codes are UPPER_SNAKE now, which is what the rest
// of the surface emits and what sdk_compat asserts.
func writeEstimateError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// /v1/estimate is gateway-native: no vendor dialect claims it, so the shape
	// is the OpenAI one. Going through the shared builder with the empty format
	// says that on purpose rather than by picking a writer, and keeps the route
	// inside the invariant the AST guard enforces.
	_, _ = w.Write(envelope.GatewayErrorBodyForIngress("", status, code, message, ""))
}
