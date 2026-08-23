package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

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

// proxy_routing.go answers "which target should serve this", once admission
// has already established who is calling (see proxy_admission.go): request
// context construction, route resolution, the requested-model passthrough,
// and quota.
//
// requestNormalizeMeta is shared with the cache stage deliberately — the two
// (here and proxyState.cacheNormalized) must never diverge on normalizer choice.
func requestNormalizeMeta(r *http.Request, ingressFormat provcore.Format, modelID string) normcore.Meta {
	return normcore.Meta{
		AdapterType:  strings.ToLower(string(ingressFormat)),
		Model:        modelID,
		ContentType:  normcore.StripContentTypeParams(r.Header.Get("Content-Type")),
		Direction:    normcore.DirectionRequest,
		EndpointPath: r.URL.Path,
	}
}

func (h *Handler) buildRequestContext(r *http.Request, vkMeta *vkauth.VKMeta, body []byte, ingressFormat provcore.Format, modelID, endpointType string) *requestcontext.RequestContext {
	b := requestcontext.NewBuilder().
		WithIdentity(vkMeta).
		WithEndpoint(endpointType).
		WithHeaders(r.Header).
		WithRawBody(body)

	if h.deps.NormalizeRegistry != nil && len(body) > 0 {
		ctx := r.Context()
		meta := requestNormalizeMeta(r, ingressFormat, modelID)
		compute := func() *normcore.NormalizedPayload {
			payload, err := h.deps.NormalizeRegistry.Normalize(ctx, body, meta)
			if err != nil {
				return nil
			}
			return &payload
		}
		// Kill-switch OFF: eager-compute (byte-identical to legacy). ON: install
		// the lazy seam so the canonical materializes only when smart routing or
		// the cache pulls it; the lean path computes it zero times.
		if h.lazyCanonical {
			b = b.WithLazyNormalize(compute)
		} else {
			b = b.WithNormalized(compute())
		}
	}
	return b.Build()
}

// smartRouteNeedsCanonical reports whether routing must materialize the request
// canonical so a smart rule can read the prompt — keyed on the requested MODEL
// (the model-only match-aware probe). The cache pulls the canonical separately
// in the cache stage, so it is NOT part of this gate. Fail-safe to true when the
// kill-switch is off or the Router lacks the probe (a false negative would
// silently route smart rules to their default model).
func (h *Handler) smartRouteNeedsCanonical(ctx context.Context, modelID string) bool {
	if !h.lazyCanonical {
		return true
	}
	probe, ok := h.deps.Router.(interface {
		RequestNeedsCanonical(context.Context, string) bool
	})
	if !ok {
		return true
	}
	return probe.RequestNeedsCanonical(ctx, modelID)
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
	// Materialize the request canonical for the router ONLY when a smart rule
	// could apply to this model (Normalized() triggers the lazy compute). The
	// lean path never computes it for routing; non-smart strategies use metadata.
	var canonReq *normcore.NormalizedPayload
	if h.smartRouteNeedsCanonical(ctx, modelID) {
		canonReq = rctxFull.Normalized()
	}
	rctx := &routingcore.RoutingContext{
		RequestedModel: routingcore.RequestedModel{ID: modelID},
		EndpointType:   endpointKind,
		VirtualKey:     vkCtx,
		Headers:        routingcore.NewSafeHeaders(rctxFull.Headers()),
		Request:        canonReq,
	}

	// Embeddings capability pre-filter: parse the embedding request
	// parameters from the canonical body so the router can apply model
	// compatibility rules.
	if rctx.EndpointType == typology.EndpointKindEmbeddings {
		body := rctxFull.RawBody()
		rctx.EmbeddingRequest = parseEmbeddingRequest(body)
	}

	// Structured output is the same shape of fact as the embedding parameters
	// above: the router needs it, and the normalized payload does not carry it.
	// Read only when a smart rule could actually pick the model — an explicitly
	// named model is the caller's own choice and its limits are the model's.
	if canonReq != nil {
		rctx.RequiresStructuredOutput = requestRequiresStructuredOutput(rctxFull.RawBody())
	}

	return h.deps.Router.ResolveTargets(ctx, rctx)
}

// resolveRouteOrPassthrough resolves the requested model to targets and, when no
// routing rule matches it (zero targets, or the empty-NoCompatibleProviderError
// the resolver returns with no rules enabled), falls back to the SAME
// requested-model passthrough resolution the ServeProxy routing stage uses (see
// resolveNoMatchPassthrough / stage_routing). Without this the parallel STT /
// video / realtime handlers 404 on a plain "serve the model I asked for"
// request whenever no explicit routing rule is authored for that model — while
// chat / image / TTS on ServeProxy route it out of the box. The parallel
// modalities are never the embeddings endpoint, so a non-empty capability
// NoCompatibleProviderError cannot occur here; any error/empty result means "no
// rule matched", which is exactly the passthrough case.
func (h *Handler) resolveRouteOrPassthrough(ctx context.Context, rctxFull *requestcontext.RequestContext, in Ingress, modelID string, endpointKind typology.EndpointKind) (*routingcore.RouteResult, error) {
	routeRes, err := h.resolveRoute(ctx, rctxFull, modelID, endpointKind)
	switch {
	case err != nil:
		// Mirror the ServeProxy routing stage (stage_routing): only an EMPTY
		// NoCompatibleProviderError — the resolver's "no rules enabled" signal —
		// degrades to the requested-model passthrough. Any other resolver error
		// (a strategy evaluation failure, a DB error inside lookupTarget) is a
		// genuine fault and MUST fail closed: silently passing through would
		// serve the requested model while dropping the admin-authored routing a
		// real error should surface.
		var ncpErr *routingcore.NoCompatibleProviderError
		if !errors.As(err, &ncpErr) || len(ncpErr.Available) > 0 {
			return nil, err
		}
		// empty NoCompatibleProviderError → fall through to passthrough
	case routeRes != nil && len(routeRes.AllTargets()) > 0:
		return routeRes, nil
	case routeRes != nil && routeRes.RuleMatchedAndResolvedNothing:
		// Same precondition ServeProxy's routing stage enforces: passthrough
		// answers "no rule applies, serve what was asked", and a rule that
		// applied and resolved nothing is the opposite situation. This entry
		// point serves the STT / video / realtime handlers, which had no such
		// check — so a compliance rule redirecting audio elsewhere, whose
		// targets were unavailable, was silently undone on exactly the
		// endpoints where it was not being watched.
		return nil, &routingFallbackError{
			status:  http.StatusServiceUnavailable,
			code:    "ROUTING_RULES_RESOLVED_NOTHING",
			message: "a routing rule applies to this request but none could resolve a target",
			hint: "Check the routing trace on this request: each rule records why it yielded. " +
				"Serving the requested model directly would bypass the rule that matched.",
		}
	}
	return h.resolveNoMatchPassthrough(ctx, modelID, rctxFull.Identity(), in, endpointKind, deferredRequest{canonical: rctxFull.Normalized, rawBody: rctxFull.RawBody})
}

// requestRequiresStructuredOutput reports whether the caller asked for an
// answer held to a JSON Schema.
//
// Only a SCHEMA counts. `json_object` asks for "some JSON" and every target
// either honours it natively or is given a system instruction that does (see
// the Anthropic codec), so it does not constrain WHICH model may serve the
// request. A schema does: the answer either matches it or the caller's parse
// fails, and one model in the pool returns prose with HTTP 200 rather than
// saying no.
//
// This reads the RAW ingress body, so it has to know every dialect the gateway
// accepts — the first version knew only the OpenAI chat spelling, which left
// `/v1/responses` and `/v1/messages` unprotected while reading as if the
// feature were complete. Each entry below is where that ingress's own codec
// puts the field:
//
//	OpenAI chat        response_format.type == "json_schema"
//	OpenAI Responses   text.format.type     == "json_schema"   (codec_responses.go:168)
//	Anthropic Messages output_config.format.type == "json_schema"
//	Gemini             generationConfig.responseSchema (a schema object, no type tag)
//
// The images and audio endpoints are the counter-case that keeps this from
// over-matching: they carry `response_format` as a STRING ("url", "mp3"), so a
// `.type` lookup misses them, and Gemini's `responseMimeType: application/json`
// WITHOUT a responseSchema is the json_object equivalent — JSON asked for, no
// schema to hold it to, no constraint on the pool.
func requestRequiresStructuredOutput(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	for _, path := range []string{
		"response_format.type",
		"text.format.type",
		"output_config.format.type",
	} {
		if gjson.GetBytes(body, path).String() == "json_schema" {
			return true
		}
	}
	return gjson.GetBytes(body, "generationConfig.responseSchema").IsObject()
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

// checkQuota performs quota enforcement and downgrade logic via the Engine.
// Returns pricing info and optional Decision.
// Sets rec.StatusCode and writes a response if quota is rejected (caller must
// check rec.StatusCode != 0).
// tokenBilled reports whether a model of this type is billed per token, and so
// whether a missing price row is a misconfiguration rather than the normal
// state of affairs.
func tokenBilled(modelType string) bool {
	return modelType != "rerank"
}

func (h *Handler) checkQuota(r *http.Request, w http.ResponseWriter, rec *audit.Record, vkMeta *vkauth.VKMeta, result *routingcore.RouteResult, body []byte, requestedModel string) (float64, float64, *quota.Decision) {
	if vkMeta == nil {
		return 0, 0, nil
	}
	if h.deps.QuotaEngine == nil {
		return 0, 0, nil
	}

	// The head of the dispatch order — the strategy's first choice when it has
	// one, and the chain's when every ranked target was filtered away. Quota
	// prices the model the request will actually reach, not the one the rule
	// nominated.
	firstTarget := result.AllTargets()[0]
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

	// gjson.GetBytes is zero-copy; gjson.ParseBytes copies the whole ~50 KB body
	// to a string per request. Read max_tokens directly with GetBytes — the
	// value is identical and the per-request body copy disappears.
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
	// The pre-check reserves the endpoint's billable units, not always tokens:
	// a rerank request priced per search unit must not reserve its documents'
	// thousands of "tokens" against a per-search rate. preCallBillableUnits
	// returns tokens for token endpoints and the endpoint's own unit otherwise,
	// so `units × price / 1M` reconciles with the post-call cost stamp.
	inputUnits, outputUnits := preCallBillableUnits(firstTarget.ModelType, body)
	estimate := quota.CostEstimate{
		EstimatedInputTokens: inputUnits,
		MaxOutputTokens:      outputUnits,
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
	// Rerank is exempt, and it is the only exemption. Cohere bills it per
	// search unit ($2 per 1000), so it HAS no per-token price to configure —
	// a standing catalog assertion deliberately requires the columns to stay
	// NULL rather than carry a fabricated number or a zero that would claim
	// the endpoint is free. Treating that as a missing price row made rerank
	// return 503 for every caller holding an application virtual key, which
	// is the key type real customers hold; personal and service keys skip the
	// cost check entirely, which is why every smoke run was blind to it.
	//
	// Only rerank. image, tts and video carry per-token approximations in the
	// catalog today and must keep failing closed when someone forgets one —
	// this is not a family exemption, and it stops being needed the moment
	// the engine learns a non-token pricing dimension.
	if !modelPriced && tokenBilled(firstTarget.ModelType) && quotaHasCostLimit(decision) {
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
			// Only ordinary targets are downgrade candidates. An armed
			// context-upgrade target was chosen for its window size, so picking
			// it on price makes it primary for a reason it was never selected
			// for — same class as the health-reorder defect.
			// Both lists are downgrade candidates. A rule whose strategy picks
			// an expensive model and whose chain names a cheap one is exactly
			// the shape a near-cap key needs; looking only at the ranked half
			// would answer "no affordable model" about a model that is right
			// there in the plan.
			all := result.AllTargets()
			downgradable := make([]routingcore.RoutingTarget, 0, len(all))
			for _, t := range all {
				if !t.ContextUpgradeOnly {
					downgradable = append(downgradable, t)
				}
			}
			modelIDs := make([]string, len(downgradable))
			for i, t := range downgradable {
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
				// idx indexes DOWNGRADABLE, which modelIDs and the pricing slice
				// were built from; indexing result.Targets would select a
				// different model whenever anything was filtered out.
				if idx >= 0 && idx < len(downgradable) {
					selected := downgradable[idx]
					// The cheapest becomes primary; the rest STAY as fallbacks.
					// Truncating to one made a transient failure terminal — a
					// second, unexplained penalty for being near a quota.
					result.Promote(selected)
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
				h.writeError(w, rec, http.StatusTooManyRequests, "QUOTA_EXCEEDED", decision.Message)
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
