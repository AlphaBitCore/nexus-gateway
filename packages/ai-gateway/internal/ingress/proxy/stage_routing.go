// stage_routing.go — the routing stage of the proxy stage chain: route
// resolution with the no-match passthrough fallback, the effective
// passthrough config resolution into the immutable ResolvedRequest, the
// cross-format target filter, the Responses-API cross-format guard, and
// the cross-format streaming pre-check. Owns proxyState.routeResult /
// resolvedReq.
package proxy

import (
	"errors"
	"net/http"

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
		// A model the key may not use is a refusal, not a routing miss.
		//
		// It must not reach the requested-model passthrough. Passthrough exists
		// for "no rule matched, serve what was asked", and what was asked is
		// exactly the thing this key is denied — falling through would answer
		// the request from the model the caller named, which is the opposite of
		// the decision just made.
		var notAllowed *routingcore.ModelNotAllowedError
		if errors.As(err, &notAllowed) {
			h.writeDetailedErr(s.w, s.rec, http.StatusForbidden, "MODEL_NOT_ALLOWED",
				notAllowed.Error(), "Use an allowed model or request a policy update")
			return false
		}

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
	if routeResult == nil || len(routeResult.AllTargets()) == 0 {
		// FIRST statement in this branch: the router-LLM call recorded on THIS
		// result is already paid for, and every exit from here loses it —
		// the swap below replaces routeResult wholesale with a fallback result
		// whose trace carries no router entries, and the error arms return
		// early after writeDetailedErr has already handed s.rec to the audit
		// pipeline. Draining up front covers all three exits at once.
		drainRouterCost(s.rec, routeResult, s.logger)

		// Passthrough answers "no rule applies — serve the model they asked
		// for". When a rule DID apply and resolved nothing, that answer is the
		// opposite of the decision the admin configured: a rule that redirects
		// gpt-4o somewhere else, whose target row is disabled, would deliver
		// gpt-4o with a 200 — the redirect silently undone and nothing in the
		// exchange saying so. The rules yielded for reasons already on the
		// routing trace; the request stops here and the operator reads them.
		if routeResult != nil && routeResult.RuleMatchedAndResolvedNothing {
			// The trace goes on the record BEFORE the refusal, because the
			// refusal's own hint sends the operator to read it. The first
			// version returned here and left the assignment further down
			// unreached, so the one request whose message said "each rule
			// records why it yielded" was the one request whose
			// traffic_event.routing_trace was NULL.
			s.rec.RoutingRuleID = routeResult.RuleID
			s.rec.RoutingRuleName = routeResult.RuleName
			if t := buildRoutingAuditTrace(routeResult); t != nil {
				s.rec.RoutingTrace = t
			}
			s.logger.Warn("a routing rule applied and resolved nothing; refusing rather than "+
				"serving the requested model", "model", s.modelID)
			h.writeDetailedErr(s.w, s.rec, http.StatusServiceUnavailable, "ROUTING_RULES_RESOLVED_NOTHING",
				"a routing rule applies to this request but none could resolve a target",
				"Check the routing trace on this request: each rule records why it yielded. "+
					"Serving the requested model directly would bypass the rule that matched.")
			return false
		}

		s.logger.Debug("no routing targets resolved; trying passthrough fallback", "model", s.modelID)
		fallbackResult, fallbackErr := h.resolveNoMatchPassthrough(s.r.Context(), s.modelID, s.vkMeta, s.resolved, typology.EndpointKind(s.endpointType), deferredRequest{canonical: s.cacheNormalized, rawBody: func() []byte { return s.body }})
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
		"targets", len(routeResult.AllTargets()),
		"ruleId", routeResult.RuleID,
		"provider", routeResult.Primary().ProviderName,
	)
	s.rec.RoutingRuleID = routeResult.RuleID
	s.rec.RoutingRuleName = routeResult.RuleName
	if t := buildRoutingAuditTrace(routeResult); t != nil {
		s.rec.RoutingTrace = t
	}
	drainRouterCost(s.rec, routeResult, s.logger)
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
	if len(routeResult.AllTargets()) > 0 {
		primaryTarget = routeResult.Primary()
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
	compat, incompatible := filterCompatibleTargets(s.resolved.BodyFormat, routeResult.AllTargets(), s.resolved.WireShape, h.deps.CanonicalBridge)
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
		h.writeNoCompatibleProvider(s.w, s.rec, s.resolved.BodyFormat, providerFormat, typology.KindFromWireShape(s.resolved.WireShape))
		return false
	}
	routeResult.Narrow(compat)

	// Phase 4.2: Responses-API cross-format guard.
	// When ingress is /v1/responses and the resolved primary target's
	// adapter does NOT natively serve responses-api, stateful fields +
	// OpenAI-native built-in tools cannot be honoured: reject the
	// request with a Responses-shape 400 envelope BEFORE the request
	// hits hooks / quota / executor.
	if s.resolved.BodyFormat == provcore.FormatOpenAIResponses &&
		len(routeResult.AllTargets()) > 0 &&
		h.deps.CanonicalBridge != nil {
		primary := routeResult.Primary()
		targetFormat := provcore.Format(primary.AdapterType)
		if !h.deps.CanonicalBridge.ServesResponses(targetFormat, primary.ServesResponsesAPI, s.body) {
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
		len(routeResult.AllTargets()) > 0 &&
		!canonicalbridge.StreamShapeCompatible(s.resolved.BodyFormat, provcore.Format(routeResult.Primary().AdapterType)) {
		h.writeCrossFormatStreamUnsupported(s.w, s.rec, string(s.resolved.BodyFormat), routeResult.Primary().AdapterType)
		return false
	}
	return true
}
