// Package executor encapsulates upstream provider dispatch with retry,
// credential resolution, and health tracking.
package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// ErrAllTargetsExhausted is returned when every target in the list has
// been tried and none produced a usable response.
var ErrAllTargetsExhausted = errors.New("executor: all targets exhausted")

// StatsRecorder is an optional hook the executor calls after each upstream
// attempt with the resolved credential and outcome. Implementations must be
// non-blocking (e.g. fire-and-forget Redis writes); a slow recorder will
// delay the request path.
type StatsRecorder interface {
	RecordAttempt(credentialID string, statusCode int, errMsg string)
}

// metricsRecord is the package-level hook the executor uses to publish
// router retry/failover counts. Production wiring (cmd/ai-gateway) sets
// this to (*metrics.Recorder).RecordRouterRetry; tests can swap it for
// a stub to assert without standing up the full opsmetrics registry.
// Default is a no-op so unit tests that ignore the metric pay nothing.
var metricsRecord = func(provider, class, outcome string) {}

// SetMetricsRecorder swaps the package-level retry-metrics emitter. Pass
// nil to silence emission. Call once at process startup; not safe to
// race with live executor invocations.
func SetMetricsRecorder(fn func(provider, class, outcome string)) {
	if fn == nil {
		metricsRecord = func(string, string, string) {}
		return
	}
	metricsRecord = fn
}

// TargetExecutor walks an ordered list of RoutingTargets, resolves
// credentials + base URL + extras via [provtarget.Resolver], dispatches
// via the matching provider adapter, and records health.
type TargetExecutor struct {
	adapters *provcore.Registry
	resolver provtarget.Resolver
	health   *store.HealthTracker
	bridge   *canonicalbridge.Bridge
	stats    StatsRecorder // optional; nil disables credential stat recording
}

// New creates a TargetExecutor. health and stats may be nil.
// bridge may be nil to preserve the legacy adapter-only translation path.
func New(adapters *provcore.Registry, resolver provtarget.Resolver, health *store.HealthTracker, bridge *canonicalbridge.Bridge) *TargetExecutor {
	return &TargetExecutor{adapters: adapters, resolver: resolver, health: health, bridge: bridge}
}

// WithStats attaches a StatsRecorder that is called after each upstream attempt.
func (e *TargetExecutor) WithStats(s StatsRecorder) *TargetExecutor {
	e.stats = s
	return e
}

// retryOnSet builds a fast-membership lookup over policy.RetryOn. A nil
// slice is treated as "retry everything" (defensive — config loader
// merges DefaultRetryPolicy on top so RetryOn should always be set);
// length-0 means "retry nothing".
func retryOnSet(p cfgpolicy.RetryPolicy) (set map[cfgpolicy.ErrorClass]struct{}, retryNothing bool) {
	if p.RetryOn == nil {
		return nil, false
	}
	if len(p.RetryOn) == 0 {
		return nil, true
	}
	set = make(map[cfgpolicy.ErrorClass]struct{}, len(p.RetryOn))
	for _, c := range p.RetryOn {
		set[c] = struct{}{}
	}
	return set, false
}

// inRetryOn returns true when class is in the policy's RetryOn set. A
// nil set + retryNothing=false means "retry everything" (only happens if
// a caller forgets to merge with DefaultRetryPolicy); retryNothing=true
// always returns false.
func inRetryOn(set map[cfgpolicy.ErrorClass]struct{}, retryNothing bool, class cfgpolicy.ErrorClass) bool {
	if retryNothing {
		return false
	}
	if set == nil {
		return true
	}
	_, ok := set[class]
	return ok
}

// Execute walks targets using base as the client-originated request,
// honoring the supplied RetryPolicy. The handler is expected to compute
// `policy = yamlDefault.MergedWith(rulePolicy)` before calling so the
// executor stays purely policy-driven.
//
// Algorithm (per spec §5.1):
//
//	for each target:
//	  for tryIdx := 1..ClampMaxAttempts(policy.MaxAttemptsPerTarget):
//	    dispatch
//	    on classSuccess           -> return
//	    on classNoFailoverNoRetry -> return (4xx surfaced; no L3)
//	    on class not in RetryOn   -> emit "failover_class_excluded", L3 failover
//	    on tryIdx == max          -> emit "exhausted", L3 failover
//	    else                      -> backoff (skip if ctx deadline imminent), retry
//
// On retried success at tryIdx > 1 emits "retried_succeeded".
//
// When bridge is configured, base.Body is in the ingress wire format
// (base.BodyFormat) and is translated to each target's wire format before
// dispatch; when bridge is nil, base is passed to adapters unchanged.
func (e *TargetExecutor) Execute(
	ctx context.Context,
	targets []routingcore.RoutingTarget,
	base provcore.Request,
	policy cfgpolicy.RetryPolicy,
) *ExecutionResult {
	return e.executeInner(ctx, targets, base, policy, nil)
}

// ExecuteWithPreparedBody is Execute with the body for targets[0]'s
// first attempt already produced by Adapter.PrepareBody. The cache
// layer calls this on a MISS so PrepareBody runs exactly once per
// request — once for cache key computation, then reused as the wire
// body sent upstream.
//
// Every attempt of targets[0] — including retries — resends the
// prepared body via Adapter.ExecuteWithBody, keeping the prepare
// stage's side channels (coercion rewrites → x-nexus-coerced, the
// codec's URLOverride) intact across transient-failure retries.
// Failover to targets[1+] goes through the regular per-target
// translation path. PrepareBody is idempotent, so resending the
// prepared bytes is byte-equivalent to re-preparing.
//
// preparedBody MUST be the bytes Adapter.PrepareBody would produce for
// targets[0]; preparedRewrites MUST be the rewrites slice from the same
// call; preparedURLOverride MUST be the URLOverride from the same call (so
// a shape-driven action URL — Gemini :batchEmbedContents — reaches the
// dispatch). Pass nil/nil/"" to fall back to Execute.
func (e *TargetExecutor) ExecuteWithPreparedBody(
	ctx context.Context,
	targets []routingcore.RoutingTarget,
	base provcore.Request,
	policy cfgpolicy.RetryPolicy,
	preparedBody []byte,
	preparedRewrites []string,
	preparedURLOverride string,
) *ExecutionResult {
	if preparedBody == nil {
		return e.Execute(ctx, targets, base, policy)
	}
	return e.executeInner(ctx, targets, base, policy, &preparedFirstAttempt{
		body:        preparedBody,
		rewrites:    preparedRewrites,
		urlOverride: preparedURLOverride,
	})
}

// preparedFirstAttempt carries the PrepareBody output for the first
// target: every attempt of targets[0] (retries included) resends this
// body + side channels via ExecuteWithBody; failover targets take the
// normal translation path. consumed marks that the primary used it
// (diagnostic; the tIdx==0 gate alone scopes it to the first target).
type preparedFirstAttempt struct {
	body        []byte
	rewrites    []string
	urlOverride string
	consumed    bool
}

func (e *TargetExecutor) executeInner(
	ctx context.Context,
	targets []routingcore.RoutingTarget,
	base provcore.Request,
	policy cfgpolicy.RetryPolicy,
	prepared *preparedFirstAttempt,
) *ExecutionResult {
	var attempts []Attempt
	maxPerTarget := cfgpolicy.ClampMaxAttempts(policy.MaxAttemptsPerTarget)
	retrySet, retryNothing := retryOnSet(policy)

	attemptCounter := 0

	// lastAttemptedClass is the terminal class of the most recent target
	// we actually attempted (not one skipped/continue'd). attemptedAny
	// distinguishes "nothing has run yet" from "a prior target ran".
	// Together they gate ContextUpgradeOnly targets: skip one only when a
	// real prior target ran and did NOT overflow. When nothing ran yet —
	// the flagged target was reordered to the front or is the only
	// survivor of a downstream filter (health rank, narrowing,
	// cross-format), all of which can orphan the ordering the smart
	// strategy emitted — treat it as an ordinary target rather than
	// silently dropping it (which would be a dead feature or a spurious
	// all-targets-exhausted 502).
	var lastAttemptedClass errClass
	attemptedAny := false

	for tIdx, target := range targets {
		if target.ContextUpgradeOnly && attemptedAny && lastAttemptedClass != classContextOverflow {
			continue
		}
		callTarget, err := e.resolver.Resolve(ctx, target.ProviderID, target.ModelID, provtarget.ResolveHints{StickyKey: base.StickyKey})
		if err != nil {
			attempts = append(attempts, Attempt{Target: target, Error: fmt.Sprintf("resolve: %v", err)})
			continue
		}
		if !callTarget.Format.Valid() {
			attempts = append(attempts, Attempt{Target: target, Error: "invalid adapter_type on provider: " + target.ProviderName})
			continue
		}
		adapter, ok := e.adapters.Get(callTarget.Format)
		if !ok {
			attempts = append(attempts, Attempt{Target: target, Error: "no adapter registered for format: " + string(callTarget.Format)})
			continue
		}

		req := base
		req.Target = callTarget

		// The call-time wire shape is the TARGET adapter's native shape for
		// this endpoint kind, NOT the caller's ingress shape. The ingress
		// shape (base.WireShape) is an internal detail once we dispatch
		// upstream — it only drives the conversion decision below and the
		// egress reshape (which reads the immutable context ingress, not this
		// req). Setting it here makes BuildURL + the codec target the right
		// wire for both the primary and every failover target, across all
		// chat-kind ingresses (openai-chat, anthropic /v1/messages, gemini).
		ingressKind := typology.KindFromWireShape(base.WireShape)
		// Native /v1/responses passthrough: when the TARGET itself serves the
		// Responses API, the request stays Responses-shape end-to-end. Responses
		// is chat-kind (KindFromWireShape→Chat), so without this guard the rewrite
		// below would flip req.WireShape to openai-chat → BuildURL targets
		// /v1/chat/completions and the verbatim Responses body (input, no messages)
		// 400s with "Missing required parameter: messages". This mirrors the
		// proxy-level needsCanonicalization=false rule and the egress
		// native-passthrough skip — all three sites must agree.
		// Detect /v1/responses ingress by BodyFormat, NOT WireShape. The
		// cache-prep leg downgrades resolved.WireShape to chat when the PRIMARY
		// target cannot serve the Responses wire (so the executor treats a
		// canonicalized primary as chat-kind), but resolved.BodyFormat stays
		// FormatOpenAIResponses. Keying nativeResponses on WireShape made a
		// responses-serving FAILOVER target (mixed target list: non-responses
		// primary, responses-serving secondary) unrecognisable — its verbatim
		// Responses body was posted to the chat URL and 400'd. BodyFormat is
		// per-request-stable, so each target decides its own passthrough.
		nativeResponses := base.BodyFormat == provcore.FormatOpenAIResponses &&
			e.bridge != nil && e.bridge.ServesResponses(callTarget.Format, callTarget.ServesResponsesAPI)
		if e.bridge != nil {
			switch {
			case nativeResponses:
				// Restore the Responses wire per-target so BuildURL targets
				// /v1/responses even when base.WireShape was downgraded to chat
				// for a non-responses primary earlier in the target list.
				req.WireShape = typology.WireShapeOpenAIResponses
			case ingressKind == typology.EndpointKindChat:
				req.WireShape = e.bridge.ChatWireShapeForTarget(callTarget.Format)
			case ingressKind == typology.EndpointKindEmbeddings:
				req.WireShape = e.bridge.EmbeddingsWireShapeForTarget(callTarget.Format)
			case ingressKind == typology.EndpointKindImageGeneration:
				req.WireShape = e.bridge.ImagesWireShapeForTarget(callTarget.Format)
			case ingressKind == typology.EndpointKindRerank:
				req.WireShape = e.bridge.RerankWireShapeForTarget(callTarget.Format)
			}
		}

		// On targets[0] when a prepared body was supplied, the prepared
		// bytes are already in callTarget.Format (PrepareBody did the
		// codec encode), so skip the bridge translation — for every
		// attempt of that target, retries included (the prepared side
		// channels must survive a transient-failure retry). Subsequent
		// targets go through the normal translation path — chat,
		// embeddings, and images each have their own canonical→
		// target-wire hub codec.
		usePrepared := tIdx == 0 && prepared != nil && !prepared.consumed && prepared.body != nil
		var bridgeURLOverride string
		// bridgeRewrites carries the per-model coercions the bridge's codec
		// encode applied (contract rules on the target codec). They must be
		// merged into the winning attempt's Coerced: the adapter's own
		// PrepareBody runs the idempotent re-entry differential on the bridged
		// body and finds nothing left to apply, so without this the
		// x-nexus-coerced signal is silently lost on every bridge-translated
		// attempt.
		var bridgeRewrites []string
		switch {
		case usePrepared:
			req.Body = prepared.body
			req.BodyFormat = callTarget.Format
		case nativeResponses:
			// Keep the verbatim Responses-shape body for /v1/responses native
			// passthrough — do NOT canonicalize to chat (would lose the
			// Responses request shape the upstream /v1/responses expects).
			req.Body = base.Body
			req.BodyFormat = base.BodyFormat
		case e.bridge != nil && base.BodyFormat != callTarget.Format:
			// Unified per-kind bridge translation (chat / embeddings / image /
			// rerank), each returning the codec side channels — urlOverride
			// (embeddings endpoint suffix) and rewrites (per-model coercions).
			tr, terr := e.bridgeTranslateForTarget(ingressKind, base, callTarget)
			if terr != nil {
				attempts = append(attempts, translateAttempt(target, terr))
				continue
			}
			if tr != nil {
				req.Body = tr.body
				req.BodyFormat = callTarget.Format
				bridgeURLOverride = tr.urlOverride
				bridgeRewrites = tr.rewrites
			}
		}

		// L2 — per-target retry loop.
		var lastErrCl cfgpolicy.ErrorClass
	attemptLoop:
		for tryIdx := 1; tryIdx <= maxPerTarget; tryIdx++ {
			// A retry must not reuse the credential whose circuit the previous
			// attempt may have just opened — see reResolveForRetry. A prepared
			// body carries the resolved model stamp, so it stays valid across a
			// same-target credential re-resolve (only the credential changes)
			// and is deliberately re-dispatched on retries too, preserving its
			// coercion + url-override side channels.
			if tryIdx > 1 {
				retryTarget, rerr := e.reResolveForRetry(ctx, target, base.StickyKey)
				if rerr != nil {
					attempts = append(attempts, Attempt{Target: target, Error: fmt.Sprintf("resolve (retry %d): %v", tryIdx, rerr)})
					metricsRecord(target.ProviderName, string(lastErrCl), "failover_no_credential")
					break attemptLoop
				}
				callTarget = retryTarget
				req.Target = callTarget
			}
			attemptCounter++
			attemptCtx := nexushttp.WithAttempt(ctx, attemptCounter)

			var outcome attemptOutcome
			switch {
			case usePrepared:
				// Every attempt of the primary target with a prepared body —
				// including retries — calls adapter.ExecuteWithBody so the
				// adapter's internal PrepareBody is skipped AND the prepare
				// stage's side channels survive: the rewrites feed the
				// x-nexus-coerced header (a coerced image request retried
				// after a transient failure must not lose its markers) and
				// the urlOverride feeds the dispatched URL (an embeddings
				// batch retry must not fall back to the single-embed URL).
				outcome = e.attemptWithBody(attemptCtx, adapter, req, target, prepared.body, prepared.rewrites, prepared.urlOverride)
				prepared.consumed = true
			case bridgeURLOverride != "" || len(bridgeRewrites) > 0:
				// Cross-format bodies translated by the bridge whose codec
				// emitted side-channel output that only exists here: the
				// embeddings endpoint-selection override (Gemini
				// :batchEmbedContents) and/or the image codec's coercion
				// rewrites. Hand the prepared body + side channels straight
				// to ExecuteWithBody so they reach the dispatched URL /
				// x-nexus-coerced header — adapter.Execute would passthrough
				// the same-format body and emit neither.
				outcome = e.attemptWithBody(attemptCtx, adapter, req, target, req.Body, bridgeRewrites, bridgeURLOverride)
			default:
				outcome = e.attempt(attemptCtx, adapter, req, target)
			}
			// Coercion markers reach the winning attempt's Coerced directly
			// from the dispatch path: both the bridge leg and every prepared
			// attempt (first attempt and retry alike) hand their rewrites to
			// attemptWithBody → adapter.ExecuteWithBody, which stamps them onto
			// the response. The default leg (adapter.Execute) re-derives its own
			// Coerced from PrepareBody's idempotent differential and carries no
			// pre-applied rewrites. A post-attempt re-merge here would therefore
			// DOUBLE the x-nexus-coerced markers on the common cross-format and
			// prepared-retry paths, so there is deliberately none.
			outcome.attempt.CredentialID = callTarget.CredentialID
			outcome.attempt.CredentialName = callTarget.CredentialName
			e.recordCredentialStats(callTarget.CredentialID, &outcome)
			attempts = append(attempts, outcome.attempt)
			// Set on every real attempt so a later ContextUpgradeOnly gate
			// reads the class of the last target actually run — resolve /
			// adapter continue paths never corrupt it.
			lastAttemptedClass = outcome.class
			attemptedAny = true

			switch outcome.class {
			case classSuccess:
				if tryIdx > 1 {
					metricsRecord(target.ProviderName, string(lastErrCl), "retried_succeeded")
				}
				outcome.execResult.Attempts = attempts
				return outcome.execResult
			case classNoFailoverNoRetry:
				outcome.execResult.Attempts = attempts
				return outcome.execResult
			case classContextOverflow:
				// Never retry the same target — the same model always
				// overflows. Fail over when another target exists (the
				// smart strategy arms a larger-context upgrade target);
				// on the last target return the provider's own error so
				// the client sees the upstream context error, not a
				// generic all-targets-exhausted.
				if tIdx == len(targets)-1 {
					outcome.execResult.Attempts = attempts
					return outcome.execResult
				}
				metricsRecord(target.ProviderName, "context_overflow", "failover_context_overflow")
				break attemptLoop
			}

			// Retryable failure path.
			lastErrCl = outcome.errCl
			if !inRetryOn(retrySet, retryNothing, outcome.errCl) {
				// L3 failover, class excluded by policy.
				metricsRecord(target.ProviderName, string(outcome.errCl), "failover_class_excluded")
				break
			}
			if tryIdx == maxPerTarget {
				// L2 budget exhausted on this target.
				metricsRecord(target.ProviderName, string(outcome.errCl), "exhausted")
				break
			}

			// Compute backoff. Bail to L3 if the parent context deadline is
			// imminent — sleeping past it would hand the client a context
			// error rather than the next-target attempt.
			backoff := computeBackoff(tryIdx, policy)
			if dl, ok := ctx.Deadline(); ok {
				if time.Until(dl) <= backoff {
					metricsRecord(target.ProviderName, string(outcome.errCl), "exhausted")
					break
				}
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return &ExecutionResult{Error: ctx.Err(), Attempts: attempts}
			}
		}
	}

	return &ExecutionResult{Error: ErrAllTargetsExhausted, Attempts: attempts}
}

// attemptOutcome captures one call attempt and the executor's classification.
