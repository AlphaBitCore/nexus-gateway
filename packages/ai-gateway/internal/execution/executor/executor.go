// Package executor encapsulates upstream provider dispatch with retry,
// credential resolution, and health tracking.
package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// Attempt records the outcome of a single upstream call.
type Attempt struct {
	Target         routingcore.RoutingTarget
	CredentialID   string
	CredentialName string
	StatusCode     int
	Error          string
	LatencyMs      int
	// RetryReason is the cfgpolicy.ErrorClass string ("network",
	// "timeout", "429", "5xx") that classified this attempt as a
	// retryable failure. Empty on success and on terminal 4xx
	// (CodeInvalidRequest / CodeAuthFailed / CodeEndpointUnsupported /
	// CodeNoCompatibleProvider). Stamped regardless of whether a retry
	// actually happened so the audit row records why each attempt would
	// have been retryable.
	RetryReason string
}

// ExecutionResult is the aggregate outcome of [TargetExecutor.Execute].
// It is decoupled from [provcore.Response] so that callers (proxy
// handler) can rely on a stable shape independent of provider changes.
type ExecutionResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte                 // non-streaming; nil for streaming
	Stream     provcore.StreamSession // streaming; nil for non-streaming
	Usage      provcore.Usage
	// Coerced lists any in-place request rewrites the adapter applied before
	// dispatching upstream, formatted as "<from>→<to>". Sourced from
	// provcore.Response.Coerced. Empty when no rewrite occurred.
	Coerced  []string
	Target   routingcore.RoutingTarget
	Attempts []Attempt
	Error    error
	// ProviderError is the canonical, normalised view of an upstream 4xx
	// (or other non-retryable provider failure). Set on the terminal
	// classNoFailoverNoRetry path so the handler can reshape the error
	// envelope into the ingress format (a client calling OpenAI
	// /v1/chat/completions must not receive an Anthropic-shaped error
	// body). Body still carries the upstream's raw bytes for the
	// same-format passthrough case. Nil on success.
	ProviderError *provcore.ProviderError
	// TargetMethod + TargetPath capture the upstream URL the executor
	// actually dispatched to — sourced from Response.TargetMethod /
	// TargetPath (success) or ProviderError.TargetMethod / TargetPath
	// (4xx/5xx). Empty for synthetic transport failures that never
	// reached the network; the handler falls back to client method/path.
	TargetMethod string
	TargetPath   string
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
// Subsequent retries on targets[0] and any failover to targets[1+]
// fall back to the regular Execute path (bridge translation +
// Adapter.PrepareBody). PrepareBody is idempotent so behaviour is
// indistinguishable from Execute; this only optimises the success-path
// common case (first attempt of the primary target succeeds).
//
// preparedBody MUST be the bytes Adapter.PrepareBody would produce for
// targets[0]; preparedRewrites MUST be the rewrites slice from the same
// call. Pass nil/nil to fall back to Execute.
func (e *TargetExecutor) ExecuteWithPreparedBody(
	ctx context.Context,
	targets []routingcore.RoutingTarget,
	base provcore.Request,
	policy cfgpolicy.RetryPolicy,
	preparedBody []byte,
	preparedRewrites []string,
) *ExecutionResult {
	if preparedBody == nil {
		return e.Execute(ctx, targets, base, policy)
	}
	return e.executeInner(ctx, targets, base, policy, &preparedFirstAttempt{
		body:     preparedBody,
		rewrites: preparedRewrites,
	})
}

// preparedFirstAttempt carries the PrepareBody output for the very first
// attempt on the first target. Once consumed, prepared.body is set to
// nil so subsequent attempts go through the normal path.
type preparedFirstAttempt struct {
	body     []byte
	rewrites []string
	consumed bool
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

	for tIdx, target := range targets {
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
		// 400s with "Missing required parameter: messages" (E56). This mirrors the
		// proxy-level needsCanonicalization=false rule and the egress
		// native-passthrough skip — all three sites must agree.
		nativeResponses := base.WireShape == typology.WireShapeOpenAIResponses &&
			e.bridge != nil && e.bridge.TargetNativelyServesResponsesAPI(callTarget.Format)
		if e.bridge != nil && !nativeResponses {
			switch ingressKind {
			case typology.EndpointKindChat:
				req.WireShape = e.bridge.ChatWireShapeForTarget(callTarget.Format)
			case typology.EndpointKindEmbeddings:
				req.WireShape = e.bridge.EmbeddingsWireShapeForTarget(callTarget.Format)
			}
		}

		// On targets[0] when a prepared body was supplied, the prepared
		// bytes are already in callTarget.Format (PrepareBody did the
		// codec encode), so skip the bridge translation. Subsequent
		// targets and retry-after-prepared-failure go through the normal
		// translation path — chat and embeddings each have their own
		// canonical→target-wire hub codec.
		usePrepared := tIdx == 0 && prepared != nil && !prepared.consumed && prepared.body != nil
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
		case e.bridge != nil && base.BodyFormat != callTarget.Format && ingressKind == typology.EndpointKindChat:
			wireBody, terr := e.bridge.IngressChatToWire(base.BodyFormat, callTarget.Format, base.Body, callTarget, base.Stream)
			if terr != nil {
				attempts = append(attempts, Attempt{Target: target, Error: fmt.Sprintf("hub translate: %v", terr)})
				continue
			}
			req.Body = wireBody
			req.BodyFormat = callTarget.Format
		case e.bridge != nil && base.BodyFormat != callTarget.Format && ingressKind == typology.EndpointKindEmbeddings:
			wireBody, terr := e.bridge.IngressEmbeddingsToWire(base.BodyFormat, callTarget.Format, base.Body, callTarget)
			if terr != nil {
				attempts = append(attempts, Attempt{Target: target, Error: fmt.Sprintf("hub translate: %v", terr)})
				continue
			}
			req.Body = wireBody
			req.BodyFormat = callTarget.Format
		}

		// L2 — per-target retry loop.
		var lastErrCl cfgpolicy.ErrorClass
		for tryIdx := 1; tryIdx <= maxPerTarget; tryIdx++ {
			attemptCounter++
			attemptCtx := nexushttp.WithAttempt(ctx, attemptCounter)

			var outcome attemptOutcome
			if usePrepared && tryIdx == 1 {
				// First attempt of the primary target with prepared
				// body — call adapter.ExecuteWithBody to skip the
				// adapter's internal PrepareBody.
				outcome = e.attemptWithBody(attemptCtx, adapter, req, target, prepared.body, prepared.rewrites)
				prepared.consumed = true
			} else {
				outcome = e.attempt(attemptCtx, adapter, req, target)
			}
			outcome.attempt.CredentialID = callTarget.CredentialID
			outcome.attempt.CredentialName = callTarget.CredentialName
			e.recordCredentialStats(callTarget.CredentialID, &outcome)
			attempts = append(attempts, outcome.attempt)

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
type attemptOutcome struct {
	attempt    Attempt
	execResult *ExecutionResult // populated on classSuccess and classNoFailoverNoRetry
	class      errClass
	errCl      cfgpolicy.ErrorClass // empty unless class is one of the retryable kinds
}

func (e *TargetExecutor) attempt(ctx context.Context, adapter provcore.Adapter, req provcore.Request, target routingcore.RoutingTarget) attemptOutcome {
	start := time.Now()
	resp, err := adapter.Execute(ctx, req)
	return e.classifyAttempt(start, resp, err, target)
}

// attemptWithBody is attempt's twin for the cache-MISS first-attempt
// path: skips Adapter.PrepareBody by calling Adapter.ExecuteWithBody
// with the body the cache layer already produced. Classification and
// outcome shape match attempt() exactly.
func (e *TargetExecutor) attemptWithBody(ctx context.Context, adapter provcore.Adapter, req provcore.Request, target routingcore.RoutingTarget, body []byte, rewrites []string) attemptOutcome {
	start := time.Now()
	resp, err := adapter.ExecuteWithBody(ctx, req, body, rewrites)
	return e.classifyAttempt(start, resp, err, target)
}

func (e *TargetExecutor) classifyAttempt(start time.Time, resp *provcore.Response, err error, target routingcore.RoutingTarget) attemptOutcome {
	latency := int(time.Since(start).Milliseconds())

	a := Attempt{
		Target:    target,
		LatencyMs: latency,
	}

	cls, errCl := classify(resp, err)
	a.RetryReason = string(errCl)

	switch cls {
	case classSuccess:
		a.StatusCode = resp.StatusCode
		e.recordHealth(target, true, latency)
		return attemptOutcome{
			attempt: a,
			class:   cls,
			execResult: &ExecutionResult{
				StatusCode:   resp.StatusCode,
				Headers:      resp.Headers,
				Body:         resp.Body,
				Stream:       resp.Stream,
				Usage:        resp.Usage,
				Coerced:      resp.Coerced,
				Target:       target,
				TargetMethod: resp.TargetMethod,
				TargetPath:   resp.TargetPath,
			},
		}
	case classNoFailoverNoRetry:
		// 4xx terminal — surface the upstream body + headers directly so
		// the handler can either pass through (ingress == upstream) or
		// reshape the envelope for a cross-format client.
		var pe *provcore.ProviderError
		if errors.As(err, &pe) {
			a.StatusCode = pe.Status
			a.Error = pe.Error()
			e.recordHealth(target, false, latency)
			return attemptOutcome{
				attempt: a,
				class:   cls,
				execResult: &ExecutionResult{
					StatusCode:    pe.Status,
					Headers:       pe.Headers,
					Body:          pe.Raw,
					Target:        target,
					ProviderError: pe,
					TargetMethod:  pe.TargetMethod,
					TargetPath:    pe.TargetPath,
				},
			}
		}
		// classifier promised a ProviderError for this class; defensive fallback.
		a.Error = "no-failover error without provider envelope"
		e.recordHealth(target, false, latency)
		return attemptOutcome{
			attempt:    a,
			class:      cls,
			execResult: &ExecutionResult{StatusCode: http.StatusInternalServerError, Target: target},
		}
	default:
		// Retryable failure (network / timeout / 429 / 5xx).
		var pe *provcore.ProviderError
		if errors.As(err, &pe) {
			a.StatusCode = pe.Status
			a.Error = pe.Error()
		} else if err != nil {
			a.Error = err.Error()
		}
		e.recordHealth(target, false, latency)
		return attemptOutcome{
			attempt: a,
			class:   cls,
			errCl:   errCl,
		}
	}
}

func (e *TargetExecutor) recordHealth(target routingcore.RoutingTarget, success bool, latencyMs int) {
	if e.health == nil {
		return
	}
	if success {
		e.health.RecordSuccess(target.ProviderID, target.ProviderName, latencyMs)
	} else {
		e.health.RecordFailure(target.ProviderID, target.ProviderName, latencyMs)
	}
}

func (e *TargetExecutor) recordCredentialStats(credID string, o *attemptOutcome) {
	if e.stats == nil || credID == "" {
		return
	}
	e.stats.RecordAttempt(credID, o.attempt.StatusCode, o.attempt.Error)
}
