// stage_routing.go — the routing stage of the proxy stage chain: route
// resolution with the no-match passthrough fallback, the effective
// passthrough config resolution into the immutable ResolvedRequest, the
// cross-format target filter, the Responses-API cross-format guard, and
// the cross-format streaming pre-check. Owns proxyState.routeResult /
// resolvedReq.
package proxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

type routingFallbackError struct {
	status  int
	code    string
	message string
	hint    string
}

func (e *routingFallbackError) Error() string {
	return e.message
}

// routingStage resolves the requested model to ordered provider+model
// targets and applies the post-routing guards.
type routingStage struct{ s *proxyState }

func (st routingStage) run() bool {
	s := st.s
	h := s.h

	// Phase 4: Routing.
	routeResult, err := h.resolveRoute(s.r.Context(), s.rctxFull, s.modelID, typology.EndpointKind(s.endpointType))
	if err != nil {
		// Capability pre-filter: all candidates were rejected for this
		// embedding request. Emit a structured 400 with
		// available_capabilities so the client knows what each model
		// supports.
		//
		// Edge case: when zero routing rules are enabled, resolver.go
		// short-circuits on the embeddings endpoint and returns an
		// empty NoCompatibleProviderError (Available=[]). Chat falls
		// through to the passthrough fallback in this case; embeddings
		// should too. An empty Available list means no candidate was
		// ever evaluated by the capability filter, so the "no
		// compatible capability" error message is misleading — try the
		// passthrough fallback instead.
		var ncpErr *routingcore.NoCompatibleProviderError
		if errors.As(err, &ncpErr) {
			if len(ncpErr.Available) > 0 {
				h.writeNoCompatibleCapability(s.w, s.rec, ncpErr)
				return false
			}
			s.logger.Debug("empty NoCompatibleProviderError; trying passthrough fallback", "model", s.modelID)
			// fall through to the no-targets passthrough path below
		} else {
			s.logger.Error("routing failed", "error", err)
			h.writeDetailedErr(s.w, s.rec, http.StatusInternalServerError, "ROUTING_NO_MATCH",
				"routing failed", "Check that a routing rule exists for this model")
			return false
		}
	}
	if routeResult == nil || len(routeResult.Targets) == 0 {
		s.logger.Debug("no routing targets resolved; trying passthrough fallback", "model", s.modelID)
		fallbackResult, fallbackErr := h.resolveNoMatchPassthrough(s.r.Context(), s.modelID, s.vkMeta, s.resolved)
		if fallbackErr != nil {
			var routingErr *routingFallbackError
			if errors.As(fallbackErr, &routingErr) {
				h.writeDetailedErr(s.w, s.rec, routingErr.status, routingErr.code, routingErr.message, routingErr.hint)
				return false
			}
			s.logger.Error("passthrough fallback failed", "model", s.modelID, "error", fallbackErr)
			h.writeDetailedErr(s.w, s.rec, http.StatusInternalServerError, "ROUTING_NO_MATCH",
				"routing fallback failed", "Check gateway model catalog and provider configuration")
			return false
		}
		routeResult = fallbackResult
	}
	s.logger.Debug("route resolved",
		"model", s.modelID,
		"targets", len(routeResult.Targets),
		"ruleId", routeResult.RuleID,
		"provider", routeResult.Targets[0].ProviderName,
	)
	s.rec.RoutingRuleID = routeResult.RuleID
	s.rec.RoutingRuleName = routeResult.RuleName
	if t := buildRoutingAuditTrace(routeResult); t != nil {
		s.rec.RoutingTrace = t
	}
	// Stamp the REQUESTED-side identity (traffic_event model_id / provider_id
	// / provider_name). These carry the model the CLIENT asked for, and are
	// populated only when that model resolved unambiguously to one catalog
	// model — for "auto" / multi-candidate / unresolved they stay empty
	// (RouteResult computes this; see RequestedModelID). They are NOT the
	// routed pick: the audit table's distinct routed_provider_id /
	// routed_model_id columns are filled by fetchUpstream / cache-HIT from the
	// actually-served RoutingTarget, and all usage/cost/analytics attribute by
	// those. rec.ModelName keeps the literal client string stamped at
	// admission ("claude-opus-4-7" / "auto") so the "Requested model" column
	// shows what the client actually wrote.
	s.rec.ModelID = routeResult.RequestedModelID
	s.rec.ProviderID = routeResult.RequestedProviderID
	s.rec.ProviderName = routeResult.RequestedProviderName
	s.routeResult = routeResult

	// Phase 4.5: resolve effective passthrough config for the primary
	// target's provider and wrap the L3 RequestContext + post-routing
	// decisions into an immutable ResolvedRequest. Stashed on
	// r.Context() so downstream consumers (hooks pipeline, audit,
	// executor) can read passthrough state without re-resolving.
	//
	// The cache is empty cold-start (fail-closed); Effective returns
	// nil until Hub pushes a real snapshot, and Resolve preserves nil.
	// Nil-receiver methods (AnyBypassActive, Flags) treat nil as
	// "no bypass".
	var primaryTarget routingcore.RoutingTarget
	if len(routeResult.Targets) > 0 {
		primaryTarget = routeResult.Targets[0]
	}
	var passthroughCfg *passthrough.Config
	if h.deps.PassthroughCache != nil {
		passthroughCfg = h.deps.PassthroughCache.Effective(primaryTarget.ProviderID, primaryTarget.AdapterType)
	}
	resolvedReq := requestcontext.Resolve(s.rctxFull, routeResult, passthroughCfg)
	s.r = s.r.WithContext(requestcontext.WithResolved(s.r.Context(), resolvedReq))
	s.resolvedReq = resolvedReq

	// Stamp the bypass flags + operator reason on the audit record
	// so every downstream branch (hooks skip, cache skip, response
	// normalize skip) writes a row whose passthrough_flags column
	// reflects which layers were bypassed. PassthroughFlags is the
	// canonical-order slice from passthrough.Config.Flags() —
	// operators grep / SQL-filter on these literals.
	if pt := resolvedReq.Passthrough(); pt.AnyBypassActive() {
		s.rec.PassthroughFlags = pt.Flags()
		s.rec.PassthroughReason = pt.Reason
	}
	s.phaseTimer.Mark(traffic.PhaseRouting)

	// Phase 4.1: Cross-format routing filter.
	// When CanonicalBridge is wired, chat completions use the OpenAI
	// hub matrix ([canonicalbridge.Bridge.EndpointRoutable]); otherwise
	// tests fall back to the legacy rule (same format or OpenAI ingress).
	compat, incompatible := filterCompatibleTargets(s.resolved.BodyFormat, routeResult.Targets, s.resolved.WireShape, h.deps.CanonicalBridge)
	if h.deps.SchemaMismatchRecorder != nil {
		for _, rt := range incompatible {
			h.deps.SchemaMismatchRecorder.RecordSchemaMismatch(string(s.resolved.BodyFormat), string(rt.ProviderFormat))
		}
	}
	if len(compat) == 0 {
		providerFormat := ""
		if len(incompatible) > 0 {
			providerFormat = string(incompatible[0].ProviderFormat)
		}
		h.writeNoCompatibleProvider(s.w, s.rec, s.resolved.BodyFormat, providerFormat)
		return false
	}
	routeResult.Targets = compat

	// Phase 4.2: Responses-API cross-format guard.
	// When ingress is /v1/responses and the resolved primary target's
	// adapter does NOT natively serve responses-api, stateful fields +
	// OpenAI-native built-in tools cannot be honoured: reject the
	// request with a Responses-shape 400 envelope BEFORE the request
	// hits hooks / quota / executor.
	if s.resolved.BodyFormat == provcore.FormatOpenAIResponses &&
		len(routeResult.Targets) > 0 &&
		h.deps.CanonicalBridge != nil {
		targetFormat := provcore.Format(routeResult.Targets[0].AdapterType)
		if !h.deps.CanonicalBridge.TargetNativelyServesResponsesAPI(targetFormat) {
			if rej := validateResponsesIngressForCrossFormat(s.body); rej != nil {
				h.writeResponsesFeatureRejection(s.w, s.rec, rej)
				return false
			}
		}
	}

	// Cross-format streaming compatibility pre-check for EVERY chat-kind
	// ingress (openai-chat, anthropic /v1/messages, gemini, responses), not
	// just openai-chat — the per-ingress SSE transcoder
	// (NewStreamTranscoder, keyed on ingress.BodyFormat) handles the
	// response re-encode, but pairs StreamShapeCompatible rejects (e.g.
	// anything involving Bedrock) must fail fast with a clear 4xx rather
	// than a messy mid-stream error.
	if s.isStream && typology.KindFromWireShape(s.resolved.WireShape) == typology.EndpointKindChat &&
		len(routeResult.Targets) > 0 &&
		!canonicalbridge.StreamShapeCompatible(s.resolved.BodyFormat, provcore.Format(routeResult.Targets[0].AdapterType)) {
		h.writeCrossFormatStreamUnsupported(s.w, s.rec, string(s.resolved.BodyFormat), routeResult.Targets[0].AdapterType)
		return false
	}
	return true
}

func (h *Handler) resolveNoMatchPassthrough(ctx context.Context, requestedModel string, vkMeta *vkauth.VKMeta, in Ingress) (*routingcore.RouteResult, error) {
	if h.deps == nil || h.deps.Models == nil {
		return nil, &routingFallbackError{
			status:  http.StatusInternalServerError,
			code:    "ROUTING_NO_MATCH",
			message: "passthrough fallback is unavailable",
			hint:    "Model lookup dependency is not configured",
		}
	}

	model, err := h.deps.Models.GetModelByCode(ctx, requestedModel)
	if err != nil || model == nil {
		return nil, &routingFallbackError{
			status:  http.StatusNotFound,
			code:    "ROUTING_NO_MATCH",
			message: "no available provider for model " + requestedModel,
			hint:    "Ensure the model exists and is enabled",
		}
	}

	if vkMeta != nil && len(vkMeta.AllowedModels) > 0 &&
		!routingcore.ModelMatchesAllowedRefs(model.ID, model.ProviderModelID, model.ProviderID, vkMeta.AllowedModels) {
		return nil, &routingFallbackError{
			status:  http.StatusForbidden,
			code:    "MODEL_NOT_ALLOWED",
			message: "model " + requestedModel + " is not allowed for this virtual key",
			hint:    "Use an allowed model or request policy update",
		}
	}

	providerName := model.ProviderName
	if providerName == "" {
		providerName = model.ProviderID
	}
	// Use the provider's actual wire adapter type so the normaliser
	// (L3/L4) and cache-key preparation use the correct format.
	// Falls back to the ingress format when adapter_type is not
	// stored (legacy rows or test doubles).
	adapterType := model.ProviderAdapterType
	if adapterType == "" {
		adapterType = string(in.BodyFormat)
	}
	target := routingcore.RoutingTarget{
		ProviderID:      model.ProviderID,
		ProviderName:    providerName,
		AdapterType:     adapterType,
		ModelID:         model.ID,
		ModelCode:       model.Code,
		ModelName:       model.Name,
		ProviderModelID: model.ProviderModelID,
		BaseURL:         model.ProviderBaseURL,
		Source:          "passthrough-fallback",
	}
	return &routingcore.RouteResult{
		Targets:  []routingcore.RoutingTarget{target},
		RuleID:   "passthrough-fallback",
		RuleName: "passthrough-fallback",
		// The client requested this specific model and no routing rule
		// substituted it — passthrough sends straight to it — so the requested
		// side IS this model. Without this, the common default deployment
		// (only smart-auto-routing enabled, so specific-model requests fall to
		// passthrough) would leave the requested columns NULL.
		RequestedModelID:      model.ID,
		RequestedProviderID:   model.ProviderID,
		RequestedProviderName: providerName,
	}, nil
}
