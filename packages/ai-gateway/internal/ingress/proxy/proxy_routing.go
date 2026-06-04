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
// /v1/estimate compare requests use a dedicated per-VK bucket
// (checkCompareRateLimit, keyed by vkName + ":compare") so estimation
// traffic cannot exhaust the real-call quota and vice versa.
func (h *Handler) checkRateLimit(w http.ResponseWriter, vkMeta *vkauth.VKMeta) error {
	if vkMeta.RateLimitRpm == nil || h.deps.RateLimiter == nil {
		return nil
	}
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.Name, *vkMeta.RateLimitRpm, 60_000)
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
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.Name+":compare", limit, 60_000)
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
	// BatchSize: input is either a string (single = 1) or array (len).
	if in := gjson.GetBytes(body, "input"); in.IsArray() {
		req.BatchSize = int(in.Get("#").Int())
		if req.BatchSize == 0 {
			req.BatchSize = 1
		}
	} else {
		req.BatchSize = 1
	}
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
	if h.deps.Models != nil {
		qModel, _ := h.deps.Models.GetModel(r.Context(), firstTarget.ModelID)
		if qModel != nil {
			if qModel.InputPricePM != nil {
				quotaInPrice = *qModel.InputPricePM
			}
			if qModel.OutputPricePM != nil {
				quotaOutPrice = *qModel.OutputPricePM
			}
		}
	}

	parsed := gjson.ParseBytes(body)
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
				// Use a budget based on remaining estimated cost.
				idx := quota.SelectCheapestIndex(pricing, estimate, estimate.EstimatedCost()*0.5)
				if idx >= 0 && idx < len(result.Targets) {
					selected := result.Targets[idx]
					result.Targets = []routingcore.RoutingTarget{selected}
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
