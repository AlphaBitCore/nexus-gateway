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
	"encoding/json"
	"fmt"
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
		writeEstimateError(w, http.StatusMethodNotAllowed, "estimate_method_not_allowed", "POST only")
		return
	}

	// VK auth — same surface as the proxy /v1/* endpoints.
	vkMeta, err := h.authenticate(r)
	if err != nil {
		writeEstimateError(w, http.StatusUnauthorized, "estimate_unauthorized", err.Error())
		return
	}

	// Separate per-VK compareEndpointRateLimit bucket.
	if err := h.checkCompareRateLimit(w, vkMeta); err != nil {
		writeEstimateError(w, http.StatusTooManyRequests, "estimate_compare_rate_limited", err.Error())
		return
	}

	var req EstimateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEstimateError(w, http.StatusBadRequest, "estimate_invalid_json", err.Error())
		return
	}
	if len(req.CompareTargets) == 0 {
		writeEstimateError(w, http.StatusBadRequest, "estimate_no_targets",
			"compareTargets array must contain at least 1 target")
		return
	}
	if len(req.CompareTargets) > 10 {
		writeEstimateError(w, http.StatusBadRequest, "estimate_too_many_targets",
			"compareTargets exceeds maximum of 10 entries per request")
		return
	}
	if len(req.Request) == 0 {
		writeEstimateError(w, http.StatusBadRequest, "estimate_no_request",
			"request body is required")
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
			writeEstimateError(w, http.StatusBadRequest, "estimate_invalid_reasoning_effort",
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
			Code:    "estimate_target_not_found",
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

	maxOutput := 0
	if m.MaxOutputTokens != nil {
		maxOutput = *m.MaxOutputTokens
	}

	in := estimator.EstimateInput{
		CanonicalRequest: body,
		IngressFormat:    provcore.FormatOpenAI,
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
			Code:    "estimate_failed",
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

func (h *Handler) resolveTargetModel(ctx context.Context, target EstimateCompareTarget) (store.Model, bool) {
	if h.deps == nil || h.deps.Models == nil {
		return store.Model{}, false
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

// writeEstimateError writes a structured per-endpoint error. (The
// proxy-style writeJSONError carries different framing — code embedded
// in `error.code` numeric — and uses `type: "proxy_error"`. We want
// `error.code` to be a stable string slug, so this is a distinct
// helper rather than a shim around writeJSONError.)
func writeEstimateError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
