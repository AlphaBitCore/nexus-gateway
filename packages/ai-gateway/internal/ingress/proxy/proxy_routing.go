package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// proxy_routing.go holds the per-request authentication, rate-limit, request-context
// build, route resolution, and quota-enforcement helpers split out of proxy.go
// (behavior unchanged). ServeProxy orchestrates these in order.

func (h *Handler) authenticate(r *http.Request) (*vkauth.VKMeta, error) {
	if h.deps.VKAuth == nil {
		return nil, fmt.Errorf("VIRTUAL_KEY_MISSING: authenticator not configured")
	}
	return h.deps.VKAuth.Authenticate(r.Context(), r)
}

// writeAuthError writes an appropriate auth error response with machine-parseable codes.
func (h *Handler) writeAuthError(w http.ResponseWriter, rec *audit.Record, err error) {
	code := "AUTH_INVALID_KEY"
	hint := "Verify your virtual key is correct"
	switch {
	case errors.Is(err, vkauth.ErrMissing):
		code = "AUTH_KEY_MISSING"
		hint = "Include a virtual key via x-nexus-virtual-key header or Authorization: Bearer"
	case errors.Is(err, vkauth.ErrDisabled):
		code = "AUTH_KEY_DISABLED"
		hint = "This key has been disabled by an administrator"
	case errors.Is(err, vkauth.ErrExpired):
		code = "AUTH_KEY_EXPIRED"
		hint = "This key has expired; request a new one from your admin"
	}
	h.writeDetailedErr(w, rec, http.StatusUnauthorized, code, err.Error(), hint)
}

// checkRateLimit checks per-key rate limits. Sets Retry-After header on rejection.
//
// The bucket is keyed on vkMeta.ID — the globally-unique VirtualKey id —
// NOT vkMeta.Name. VirtualKey.name has no uniqueness constraint, so two
// tenants that happen to pick the same display label would otherwise share
// one Redis bucket (`nexus:rl:<name>`) and exhaust each other's budget.
//
// /v1/estimate compare requests use a dedicated per-VK bucket
// (checkCompareRateLimit, keyed by the VK id + ":compare") so estimation
// traffic cannot exhaust the real-call quota and vice versa.
func (h *Handler) checkRateLimit(w http.ResponseWriter, vkMeta *vkauth.VKMeta) error {
	if vkMeta.RateLimitRpm == nil || h.deps.RateLimiter == nil {
		return nil
	}
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.ID, *vkMeta.RateLimitRpm, 60_000)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		return fmt.Errorf("rate limit exceeded")
	}
	return nil
}

// compareEndpointRateLimitDefault is the per-VK fallback when
// CompareEndpointRateLimitRpm is NULL.
const compareEndpointRateLimitDefault = 30

func (h *Handler) checkCompareRateLimit(w http.ResponseWriter, vkMeta *vkauth.VKMeta) error {
	if h.deps.RateLimiter == nil {
		return nil
	}
	limit := compareEndpointRateLimitDefault
	if vkMeta.CompareEndpointRateLimitRpm != nil {
		limit = *vkMeta.CompareEndpointRateLimitRpm
	}
	if limit <= 0 {
		return nil
	}
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.ID+":compare", limit, 60_000)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		return fmt.Errorf("compare-endpoint rate limit exceeded")
	}
	return nil
}

// buildRequestContext constructs the L3 request context. It performs
// exactly one normcore.Registry.Normalize call per request (skipped for
// empty bodies) and packages the canonical NormalizedPayload alongside
// identity, endpoint, headers, and raw body into an immutable
// *requestcontext.RequestContext. Downstream L4 consumers (routing,
// hooks, audit) read from this single artefact instead of re-parsing
// raw bytes.
//
// Normalize errors are swallowed: the canonical payload remains nil and
// routing/hooks fall back to their nil-Request behaviour. A malformed
// or unrecognised body must not block the request — the routing layer
// makes its own non-smart fallback.
func (h *Handler) buildRequestContext(r *http.Request, vkMeta *vkauth.VKMeta, body []byte, ingressFormat provcore.Format, modelID, endpointType string) *requestcontext.RequestContext {
	var canonical *normcore.NormalizedPayload
	if h.deps.NormalizeRegistry != nil && len(body) > 0 {
		payload, err := h.deps.NormalizeRegistry.Normalize(r.Context(), body, normcore.Meta{
			AdapterType:  string(ingressFormat),
			Model:        modelID,
			ContentType:  r.Header.Get("Content-Type"),
			Direction:    normcore.DirectionRequest,
			EndpointPath: r.URL.Path,
		})
		if err == nil {
			canonical = &payload
		}
	}
	return requestcontext.NewBuilder().
		WithIdentity(vkMeta).
		WithNormalized(canonical).
		WithEndpoint(endpointType).
		WithHeaders(r.Header).
		WithRawBody(body).
		Build()
}

// resolveRoute runs the routing engine via Router.ResolveTargets, returning a
// flat RouteResult with targets already health-ranked. The router input is
// built from the RequestContext; the canonical Normalized payload flows
// through rctx.Request so smart routing can inspect the user prompt.
//
// For embeddings requests, the raw canonical body is also parsed into an
// EmbeddingRequest so the capability pre-filter can apply before target
// dispatch.
func (h *Handler) resolveRoute(ctx context.Context, rctxFull *requestcontext.RequestContext, modelID string, endpointKind typology.EndpointKind) (*routingcore.RouteResult, error) {
	var vkCtx *routingcore.VKContext
	if vkMeta := rctxFull.Identity(); vkMeta != nil {
		orgPath := buildOrgPath(vkMeta.OrganizationID, h.orgParents())
		vkCtx = &routingcore.VKContext{
			ID:               vkMeta.ID,
			Name:             vkMeta.Name,
			OrganizationID:   vkMeta.OrganizationID,
			OrganizationPath: orgPath,
			ProjectID:        vkMeta.ProjectID,
			SourceApp:        vkMeta.SourceApp,
			AllowedModels:    vkMeta.AllowedModels,
		}
	}
	rctx := &routingcore.RoutingContext{
		RequestedModel: routingcore.RequestedModel{ID: modelID},
		EndpointType:   endpointKind,
		VirtualKey:     vkCtx,
		Headers:        routingcore.NewSafeHeaders(rctxFull.Headers()),
		Request:        rctxFull.Normalized(),
	}

	// Embeddings capability pre-filter: parse the embedding request
	// parameters from the canonical body so the router can apply model
	// compatibility rules.
	if rctx.EndpointType == typology.EndpointKindEmbeddings {
		body := rctxFull.RawBody()
		rctx.EmbeddingRequest = parseEmbeddingRequest(body)
	}

	return h.deps.Router.ResolveTargets(ctx, rctx)
}

// parseEmbeddingRequest extracts the embedding request parameters from
// the canonical body (OpenAI-compatible shape). All fields are optional;
// absent fields are left at zero values (nil pointers / empty strings).
func parseEmbeddingRequest(body []byte) *routingcore.EmbeddingRequestParams {
	if len(body) == 0 {
		return &routingcore.EmbeddingRequestParams{BatchSize: 1}
	}
	req := &routingcore.EmbeddingRequestParams{}
	if d := gjson.GetBytes(body, "dimensions"); d.Exists() {
		v := int(d.Int())
		req.Dimensions = &v
	}
	if e := gjson.GetBytes(body, "encoding_format").String(); e != "" {
		req.EncodingFormat = e
	}
	req.InputType = canonicalext.Get(body, "cohere", "input_type").String()
	req.TaskType = canonicalext.Get(body, "gemini", "taskType").String()
	// BatchSize: single string / single token-id sequence = 1; an array of
	// strings or an array of token-id sequences = its length.
	req.BatchSize = embeddingBatchSize(gjson.GetBytes(body, "input"))
	return req
}

func (h *Handler) orgParents() map[string]string {
	if h.deps == nil || h.deps.QuotaEngine == nil {
		return nil
	}
	return h.deps.QuotaEngine.OrgParents()
}

func buildOrgPath(orgID string, parents map[string]string) []string {
	if orgID == "" || len(parents) == 0 {
		return nil
	}
	path := make([]string, 0, 4)
	current := orgID
	for current != "" {
		parent := parents[current]
		if parent == "" {
			break
		}
		path = append(path, parent)
		current = parent
	}
	return path
}

// estimateTokens estimates the number of tokens in a request body using
// rune-based counting. CJK characters are single runes but often correspond
// to more tokens than ASCII bytes/4 would suggest; rune/3 gives a better
// cross-language approximation.
func estimateTokens(body []byte) int64 {
	runeCount := int64(utf8.RuneCount(body))
	est := runeCount / 3
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

// checkQuota performs quota enforcement and downgrade logic via the Engine.
// Returns pricing info and optional Decision.
// Sets rec.StatusCode and writes a response if quota is rejected (caller must
// check rec.StatusCode != 0).
func (h *Handler) checkQuota(r *http.Request, w http.ResponseWriter, rec *audit.Record, vkMeta *vkauth.VKMeta, result *routingcore.RouteResult, body []byte, requestedModel string) (float64, float64, *quota.Decision) {
	if vkMeta == nil {
		return 0, 0, nil
	}
	if h.deps.QuotaEngine == nil {
		return 0, 0, nil
	}

	firstTarget := result.Targets[0]
	var quotaInPrice, quotaOutPrice float64
	// modelPriced tracks whether the routed model has a pricing row at all.
	// We distinguish "unpriced" (no price set — InputPricePM and
	// OutputPricePM both nil) from "free" (price explicitly 0): an unpriced
	// model estimates $0 and silently bypasses every cost cap,
	// whereas a free model should be allowed. Defaults to true so a missing
	// Models dependency or a transient lookup error fails OPEN (consistent
	// with the quota subsystem's fail-open posture) rather than rejecting
	// every request.
	modelPriced := true
	if h.deps.Models != nil {
		qModel, qErr := h.deps.Models.GetModel(r.Context(), firstTarget.ModelID)
		if qErr == nil {
			modelPriced = qModel != nil && (qModel.InputPricePM != nil || qModel.OutputPricePM != nil)
			if qModel != nil {
				if qModel.InputPricePM != nil {
					quotaInPrice = *qModel.InputPricePM
				}
				if qModel.OutputPricePM != nil {
					quotaOutPrice = *qModel.OutputPricePM
				}
			}
		}
	}

	// When the routed model has no price row configured, the
	// estimated cost lands at $0 indistinguishably from a free model or a
	// failed request. Stamp metadata.cost.unpriced=true here — the only
	// place with the nil-vs-explicit-0 price distinction — so cost surfaces
	// can show "$0 because no price is set" rather than silently reporting
	// no spend. Independent of token count and of whether a cost cap
	// applies; a model priced at 0 (genuinely free) is NOT flagged.
	if !modelPriced {
		rec.Metadata = stampUnpricedCost(rec.Metadata)
	}

	parsed := gjson.ParseBytes(body)
	// Output-token reservation for the quota PRE-check. This is a soft,
	// deliberately-conservative reservation, NOT the billed amount:
	//   - When the caller pins max_tokens we reserve exactly that (the
	//     provider cannot exceed it), which over-reserves whenever the real
	//     completion is shorter — the safe direction for a cost cap.
	//   - When max_tokens is omitted we reserve a fixed 4096-token default
	//     because the true ceiling is unknown pre-call; a real completion
	//     longer than 4096 would be under-reserved at pre-check, but the
	//     post-call Reconcile corrects the counter to the actual usage, so
	//     the only window is a single in-flight request.
	// Combined with the rune/3 input heuristic in estimateTokens, the
	// pre-check is an approximation; the authoritative cost is always the
	// reconciled actual usage. See §6 of
	// docs/developers/architecture/cross-cutting/safety/quota-architecture.md.
	maxTokens := parsed.Get("max_tokens").Int()
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	estimate := quota.CostEstimate{
		EstimatedInputTokens: estimateTokens(body),
		MaxOutputTokens:      maxTokens,
		InputPricePM:         quotaInPrice,
		OutputPricePM:        quotaOutPrice,
	}

	chain := quota.BuildCheckChain(vkMeta, h.deps.QuotaEngine.OrgParents())
	decision := h.deps.QuotaEngine.Check(r.Context(), chain, estimate, vkMeta)

	// An unpriced model estimates $0, so the pre-check never trips
	// and reconcile adds nothing — the model bypasses every cost cap with no
	// signal. When a cost limit is actually enforced for this caller, fail
	// closed instead of serving unaccounted spend. Free models (price set to
	// 0) are unaffected — only a missing price row triggers this.
	if !modelPriced && quotaHasCostLimit(decision) {
		logger := h.deps.Logger.With("model", firstTarget.ModelID, "vk", vkMeta.ID)
		logger.Warn("quota: routed model has no price configured; rejecting under an active cost quota")
		// 503, not 429: this is a server-side misconfiguration (a missing price
		// row the operator must add), not the caller exceeding a rate/quota they
		// could back off from — a 429 would mislead the client into retrying.
		h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "QUOTA_MODEL_UNPRICED",
			"routed model has no price configured; cost quota cannot be enforced",
			"Ask an admin to set this model's pricing before it can be used under a cost quota")
		return quotaInPrice, quotaOutPrice, decision
	}

	if !decision.Allowed {
		if decision.Action == "reject" {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "QUOTA_EXCEEDED",
				decision.Message, "Check usage or request a quota increase")
			return quotaInPrice, quotaOutPrice, decision
		}
		if decision.Action == "downgrade" {
			modelIDs := make([]string, len(result.Targets))
			for i, t := range result.Targets {
				modelIDs[i] = t.ModelID
			}
			storePricing, pErr := h.deps.Models.FetchModelPricing(r.Context(), modelIDs)
			if pErr == nil {
				pricing := quota.TargetPricingFromStore(storePricing)
				// The downgrade budget is the remaining headroom under
				// the tightest enforced cap — NOT an arbitrary 0.5×estimate
				// (which could pick a model that still blows the cap, or reject
				// when a cheaper one would fit). A downgraded model must fit
				// beneath EVERY enforced level, so use the minimum of
				// (LimitCents-CurrentCents) across all levels that carry a limit.
				budget := quotaDowngradeBudget(decision)
				idx := quota.SelectCheapestIndex(pricing, estimate, budget)
				if idx >= 0 && idx < len(result.Targets) {
					selected := result.Targets[idx]
					result.Targets = []routingcore.RoutingTarget{selected}
					// Re-resolve the quota prices from the model we
					// actually downgraded TO. Without this, Reconcile increments
					// the quota counter and rec.EstimatedCostUsd uses the
					// ORIGINAL (more expensive) model's price → over-throttle +
					// overstated billed cost that never self-corrects.
					for _, tp := range pricing {
						if tp.ModelID == selected.ModelID {
							quotaInPrice = tp.InputPricePM
							quotaOutPrice = tp.OutputPricePM
							break
						}
					}
					w.Header().Set("X-Nexus-Quota-Downgrade", "true")
					w.Header().Set("X-Nexus-Quota-Original-Model", requestedModel)
					decision.Allowed = true // Allow with downgraded model.
				} else {
					h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "QUOTA_EXCEEDED",
						"quota exceeded, no affordable model available",
						"All models exceed remaining budget; request a quota increase")
					return quotaInPrice, quotaOutPrice, decision
				}
			} else {
				h.writeError(w, rec, http.StatusTooManyRequests, decision.Message)
				return quotaInPrice, quotaOutPrice, decision
			}
		}
	} else if decision.Action == "notify-and-proceed" {
		w.Header().Set("X-Nexus-Quota-Warning", decision.Message)
	}

	// Emit VK-level quota visibility headers from the chain entry the
	// engine stamped during Check. Skip when no VK-level policy/override
	// matched so clients don't see misleading zeros.
	for _, lvl := range decision.Levels {
		if lvl.TargetType == "virtual_key" && lvl.HasLimit {
			w.Header().Set("X-Nexus-Quota-Used", fmt.Sprintf("%.2f", float64(lvl.CurrentCents)/100))
			w.Header().Set("X-Nexus-Quota-Limit", fmt.Sprintf("%.2f", float64(lvl.LimitCents)/100))
			break
		}
	}

	return quotaInPrice, quotaOutPrice, decision
}
