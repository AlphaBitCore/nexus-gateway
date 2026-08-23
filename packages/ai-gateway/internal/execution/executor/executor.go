// Package executor encapsulates upstream provider dispatch with retry,
// credential resolution, and health tracking.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// ErrNoTargetDispatchable is returned when the walk ended without a single
// attempt reaching a provider: every target failed while being prepared
// locally. It is deliberately distinct from [ErrAllTargetsExhausted], which
// means providers were called and all of them failed. The two look identical
// in a log and are opposite in what they ask an operator to do — one is an
// upstream incident, the other is a credential or catalog row on our side.
//
// Observed on 2026-08-19 across two deployments: 1,195 of 1,235 5xx events had
// no upstream round trip at all (moonshot credentials failing AES-GCM
// authentication after a key change, a cohere provider with no credential row)
// and every one of them was reported as "all upstream providers failed".
var ErrNoTargetDispatchable = errors.New("executor: no target could be dispatched")

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
	logger   *slog.Logger  // optional; nil disables the dispatch-order invariant
}

// New creates a TargetExecutor. health and stats may be nil.
// bridge may be nil to preserve the legacy adapter-only translation path.
func New(adapters *provcore.Registry, resolver provtarget.Resolver, health *store.HealthTracker, bridge *canonicalbridge.Bridge) *TargetExecutor {
	return &TargetExecutor{adapters: adapters, resolver: resolver, health: health, bridge: bridge}
}

// WithLogger attaches the logger the dispatch-order invariant reports through.
// Nil (the default) disables the check entirely — it is an observation, never a
// behaviour, and a caller that wires no logger must not pay for it.
func (e *TargetExecutor) WithLogger(l *slog.Logger) *TargetExecutor {
	e.logger = l
	return e
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

// Execute walks targets under the supplied RetryPolicy, already merged by the
// handler. Which target comes next is selectNext's decision, made from the
// class of the failure that just happened; see classify.go. With a bridge
// configured, base.Body is translated per target.
func (e *TargetExecutor) Execute(
	ctx context.Context,
	targets []routingcore.RoutingTarget,
	base provcore.Request,
	policy cfgpolicy.RetryPolicy,
) *ExecutionResult {
	return e.executeInner(ctx, targets, base, policy, nil)
}

// ExecuteWithPreparedBody is Execute with targets[0]'s body already produced by
// PrepareBody, so a cache MISS prepares once. Every attempt of that target
// resends those bytes, keeping the prepare stage's side channels intact across
// retries. The three prepared arguments MUST come from one call; nil/nil/""
// falls back to Execute.
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

	// These gate ContextUpgradeOnly targets: skip one only when a real prior
	// target ran and did NOT overflow. Reordered to the front, or the only
	// survivor of a downstream filter, it is treated as ordinary rather than
	// dropped into a dead feature or a spurious 502.
	var lastAttemptedClass errClass
	attemptedAny := false

	// Position carries meaning — capability, health and the strategy all express
	// themselves as order — so every target passed over must have been passed
	// over for a reason this loop can name. WARN only: a check that refused
	// would take down routing to report a logging gap.
	walk := newWalk(targets, maxPerTarget)

	// eliminatedProviders holds providers whose credential the upstream
	// rejected. The rejection is a property of the provider, not of the
	// request, so every later target sitting on it would spend a call to be
	// told the same thing.
	eliminatedProviders := map[string]struct{}{}

	// When nothing served, the LAST attempt's outcome is the answer. Keeping the
	// last TERMINAL failure instead returns an early target's envelope (an
	// overflow 400 surfacing over a rate limit, which no SDK retries) and pairs
	// it with another attempt's credential on the audit row.
	var lastDispatched attemptOutcome

	// Two ceilings, checked before every dispatch: spend in upstream calls and
	// patience in wall-clock. Expensive targets exhaust the first, unresponsive
	// ones the second, so neither substitutes for the other.
	callBudget := cfgpolicy.EffectiveCallBudget(policy, len(targets))
	callsSpent := 0
	var walkDeadline time.Time
	if policy.MaxWalkDuration > 0 {
		walkDeadline = time.Now().Add(policy.MaxWalkDuration)
	}
	outOfRoom := func() string {
		if callsSpent >= callBudget {
			return "upstream call budget spent"
		}
		if !walkDeadline.IsZero() && !time.Now().Before(walkDeadline) {
			return "walk deadline reached"
		}
		return ""
	}

	// The walk is driven by selectNext rather than by position, so the reason
	// the last attempt failed decides which target comes next. selectionReason
	// records that decision on the attempt: without it, a jump that skipped
	// three entries is indistinguishable from a bug.
	var lastClass errClass
	lastProviderID := ""

	for {
		tIdx, selectionReason := selectNext(walk, lastClass, lastProviderID)
		if tIdx < 0 {
			break
		}
		// Coming round to a target that already failed means resting it was
		// the point, so the rest has to be real: without a pause the walk
		// ping-pongs between two struggling providers at full speed, adding
		// load to exactly the upstreams that asked to be left alone.
		if walk[tIdx].attempts > 0 && outOfRoom() == "" {
			proceed, cerr := waitOut(ctx, &walk[tIdx], policy)
			if cerr != nil {
				return &ExecutionResult{Error: cerr, Attempts: attempts}
			}
			if !proceed {
				attempts = append(attempts, ceilingStopEntries(walk, targets, tIdx, selectionReason,
					"the caller's deadline arrives before this target could rest")...)
				break
			}
		}
		walk[tIdx].attempts++
		target := targets[tIdx]

		if reason := outOfRoom(); reason != "" {
			// BREAK, not continue: out of room does not make the next candidate
			// eligible, and continuing spun forever.
			walk[tIdx].attempts--
			attempts = append(attempts, ceilingStopEntries(walk, targets, tIdx, selectionReason, reason)...)
			break
		}
		if _, dead := eliminatedProviders[target.ProviderID]; dead {
			walk[tIdx].explained = true
			attempts = append(attempts, Attempt{
				Target:          target,
				SelectionReason: selectionReason,
				Error:           "skipped: this provider already refused this request at the account level (credentials or budget)",
			})
			continue
		}
		if target.ContextUpgradeOnly && attemptedAny && lastAttemptedClass != classContextOverflow {
			// The one skip that records no Attempt, so it records here — else
			// it reads as a target that vanished.
			walk[tIdx].explained = true
			walk[tIdx].eliminated = true
			continue
		}
		callTarget, err := e.resolver.Resolve(ctx, target.ProviderID, target.ModelID, provtarget.ResolveHints{StickyKey: base.StickyKey})
		if err != nil {
			walk[tIdx].explained = true
			attempts = append(attempts, Attempt{Target: target, SelectionReason: selectionReason, Error: fmt.Sprintf("resolve: %v", err)})
			walk[tIdx].eliminated = true
			continue
		}
		if !callTarget.Format.Valid() {
			walk[tIdx].explained = true
			attempts = append(attempts, Attempt{Target: target, SelectionReason: selectionReason, Error: "invalid adapter_type on provider: " + target.ProviderName})
			walk[tIdx].eliminated = true
			continue
		}
		adapter, ok := e.adapters.Get(callTarget.Format)
		if !ok {
			walk[tIdx].explained = true
			attempts = append(attempts, Attempt{Target: target, SelectionReason: selectionReason, Error: "no adapter registered for format: " + string(callTarget.Format)})
			walk[tIdx].eliminated = true
			continue
		}

		// Only meaningful when selection was positional: once it follows the
		// failure, skipping is the mechanism rather than the anomaly, and an
		// invariant that cries on healthy requests stops being read.
		if selectionReason == "next-in-list" {
			e.checkDispatchOrder(walk, tIdx)
		}
		walk[tIdx].explained = true

		req := base
		req.Target = callTarget

		// The call-time wire shape is the TARGET's native shape for this
		// endpoint kind, not the caller's ingress shape — that one only drives
		// the conversion decision below and the egress reshape. Setting it here
		// makes BuildURL and the codec target the right wire for the primary
		// and every failover, across all chat-kind ingresses.
		ingressKind := typology.KindFromWireShape(base.WireShape)
		// A target that serves the Responses API keeps that shape end to end.
		// Responses is chat-kind, so without this the rewrite below flips the
		// wire to chat and the verbatim body 400s on "Missing required
		// parameter: messages". Keyed on BodyFormat, not WireShape: the
		// cache-prep leg downgrades WireShape when the PRIMARY cannot serve
		// Responses, which made a responses-serving FAILOVER unrecognisable.
		nativeResponses := base.BodyFormat == provcore.FormatOpenAIResponses &&
			e.bridge != nil && e.bridge.ServesResponses(callTarget.Format, callTarget.ServesResponsesAPI, base.Body)
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

		// Prepared bytes are already in the target's format, so the bridge is
		// skipped for every attempt of targets[0] — its side channels must
		// survive a retry. Later targets take the normal translation path.
		usePrepared := tIdx == 0 && prepared != nil && !prepared.consumed && prepared.body != nil
		var bridgeURLOverride string
		// The bridge codec's per-model coercions. Uncarried, the adapter's own
		// PrepareBody re-runs its differential on the already-bridged body,
		// finds nothing, and x-nexus-coerced is lost on every bridged attempt.
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
				// Deterministic for this request: the same body fails the
				// same translation every time.
				attempts = append(attempts, translateAttempt(target, terr))
				walk[tIdx].eliminated = true
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
		// The loop draws from the target's REQUEST-level allowance, so a turn
		// the walk grants later cannot hand it a fresh one.
		for tryIdx := 1; walk[tIdx].dispatchesLeft > 0; tryIdx++ {
			// Same-target attempts count against the budget too. Exempting
			// them would let a budget of 1 permit five billed generations,
			// which defeats the only number that claims to bound spend.
			if reason := outOfRoom(); reason != "" {
				// lastErrCl is empty on the FIRST pass: nothing has failed yet,
				// the ceiling simply arrived first. Empty is a legal Prometheus
				// label value and a useless one — it reads as a class the
				// classifier forgot to set rather than as a stop that had no
				// failure behind it, and it does not group with anything.
				class := string(lastErrCl)
				if class == "" {
					class = "none"
				}
				metricsRecord(target.ProviderName, class, "exhausted")
				attempts = append(attempts, Attempt{
					Target: target, SelectionReason: selectionReason,
					Error: "stopped: " + reason,
				})
				break attemptLoop
			}
			callsSpent++
			walk[tIdx].dispatchesLeft--
			// A retry must not reuse the credential whose circuit the previous
			// attempt may have just opened — see reResolveForRetry. A prepared
			// body carries the resolved model stamp, so it stays valid across a
			// same-target credential re-resolve (only the credential changes)
			// and is deliberately re-dispatched on retries too, preserving its
			// coercion + url-override side channels.
			if tryIdx > 1 {
				retryTarget, rerr := e.reResolveForRetry(ctx, target, base.StickyKey)
				if rerr != nil {
					// No credential means this target cannot be dispatched to,
					// and the failure that sent us here often closed that door:
					// one 429 opens the circuit, so the target that just
					// rate-limited is the one whose resolve now fails. Left
					// deprioritised it came round forever, spending no budget.
					attempts = append(attempts, Attempt{Target: target, Error: fmt.Sprintf("resolve (retry %d): %v", tryIdx, rerr)})
					walk[tIdx].eliminated = true
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
				// ExecuteWithBody so the prepare stage's side channels survive
				// a retry: rewrites feed x-nexus-coerced, urlOverride feeds the
				// dispatched URL (an embeddings batch retry must not fall back
				// to the single-embed endpoint).
				outcome = e.attemptWithBody(attemptCtx, adapter, req, target, prepared.body, prepared.rewrites, prepared.urlOverride)
				prepared.consumed = true
			case bridgeURLOverride != "" || len(bridgeRewrites) > 0:
				// The bridge's codec emitted side channels that exist only here
				// — an endpoint override (Gemini :batchEmbedContents) or image
				// coercion rewrites. adapter.Execute would passthrough the
				// same-format body and emit neither.
				outcome = e.attemptWithBody(attemptCtx, adapter, req, target, req.Body, bridgeRewrites, bridgeURLOverride)
			default:
				outcome = e.attempt(attemptCtx, adapter, req, target)
			}
			// Coercion markers already reach the attempt from the dispatch path,
			// so a re-merge here would DOUBLE x-nexus-coerced on the
			// cross-format and prepared-retry paths.
			outcome.attempt.CredentialID = callTarget.CredentialID
			outcome.attempt.CredentialName = callTarget.CredentialName
			e.recordCredentialStats(callTarget.CredentialID, &outcome)
			attempts = append(attempts, outcome.attempt)
			// Set on every real attempt so a later ContextUpgradeOnly gate
			// reads the class of the last target actually run — resolve /
			// adapter continue paths never corrupt it.
			lastAttemptedClass = outcome.class
			attemptedAny = true
			lastClass = outcome.class
			lastProviderID = target.ProviderID
			walk[tIdx].restable = inRetryOn(retrySet, retryNothing, outcome.errCl)
			if outcome.class != classSuccess {
				walk[tIdx].lastFailure = outcome.errCl
			}
			// Elimination is decided by the class, in one place. The arms below
			// each act on their own class, and each used to set this flag for
			// itself — so a class added to eliminatesTarget() would surface the
			// upstream envelope (which reads the predicate) and never be
			// eliminated, leaving selectNext free to pick it again until the
			// budget ran out.
			if outcome.class.eliminatesTarget() {
				walk[tIdx].eliminated = true
			}
			lastDispatched = outcome
			if n := len(attempts); n > 0 {
				attempts[n-1].SelectionReason = selectionReason
			}

			switch {
			case outcome.class == classSuccess:
				// Counted from the target's own dispatch history, not from the
				// position within this turn: with a rate limit handed back to
				// the walk, the retry that succeeds is usually a LATER TURN, and
				// a per-turn index cannot see it.
				if walk[tIdx].dispatchedBefore() {
					metricsRecord(target.ProviderName, string(walk[tIdx].lastFailure), "retried_succeeded")
				}
				outcome.execResult.Attempts = attempts
				return outcome.execResult

			case outcome.class.abortsRequest():
				// The request itself is what failed. Another target would
				// reject it identically, so trying one buys a second
				// upstream charge and the same answer.
				outcome.execResult.Attempts = attempts
				return outcome.execResult

			case outcome.class == classLocalProcessing:
				// The upstream answered and charged; the failure is ours. Never
				// retried in place, and for endpoints producing fresh output
				// the walk stops entirely — an image the caller never receives
				// is still one they pay for, and a flaky decode would buy one
				// from every target.
				metricsRecord(target.ProviderName, outcome.attempt.Code, "local_processing_failed")
				if generatesFreshOutput(typology.KindFromWireShape(base.WireShape)) {
					outcome.execResult.Attempts = attempts
					return outcome.execResult
				}
				break attemptLoop

			case outcome.class == classAuthFailed ||
				outcome.class == classPermissionDenied ||
				outcome.class == classTargetUnsupported ||
				outcome.class == classProviderQuotaExhausted:
				// Permanent for this request and scoped to this target — or,
				// for a rejected credential and a spent account budget, to
				// their provider. The old grouping ended the whole request on
				// an expired key while healthy targets waited behind it. The
				// upstream's envelope is kept, so if nothing else serves the
				// caller still sees what it said — which for an exhausted
				// budget is the only message naming when access returns.
				if outcome.class == classAuthFailed ||
					outcome.class == classProviderQuotaExhausted {
					// Both are ACCOUNT-scoped, not target-scoped: every model
					// behind that provider shares the key and the budget, so
					// dispatching a sibling spends an upstream call to be told
					// the same thing.
					eliminatedProviders[target.ProviderID] = struct{}{}
				}
				metricsRecord(target.ProviderName, outcome.attempt.Code, "eliminated_target")
				break attemptLoop

			case outcome.class == classContextOverflow:
				// The same prompt overflows the same model every time, so the
				// target is out and selectNext reaches for the largest window
				// left. With nothing left the provider's own error reaches the
				// caller, naming the real limit. "Nothing left" is eligibility,
				// not position — an index test surfaces the wrong error when
				// the overflow lands on the final entry of a list with untried
				// members ahead of it.
				if next, _ := selectNext(walk, outcome.class, target.ProviderID); next < 0 {
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
			if outcome.errCl == cfgpolicy.ErrorClassRate429 && outcome.class.wantsElapsedTime() {
				// Hand back to the walk rather than retry in place. Another
				// dispatch milliseconds from now meets the same exhausted quota;
				// coming back after two other providers have been tried does
				// not. The allowance this target did not spend is what lets the
				// walk return to it.
				metricsRecord(target.ProviderName, string(outcome.errCl), "failover_wants_elapsed_time")
				break
			}
			if walk[tIdx].dispatchesLeft <= 0 {
				// This target has spent its allowance for the whole request.
				metricsRecord(target.ProviderName, string(outcome.errCl), "exhausted")
				break
			}

			// Compute backoff. Bail to L3 if the parent context deadline is
			// imminent — sleeping past it would hand the client a context
			// error rather than the next-target attempt.
			backoff := walk[tIdx].nextBackoff(policy)
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

	// Nothing served. When the last attempt to run carried an upstream
	// envelope, that envelope is the honest answer: a rejected credential
	// surfaced as "all upstream providers failed" reads as an outage, and SDKs
	// retry outages while they never retry an authentication error.
	if lastDispatched.execResult != nil {
		lastDispatched.execResult.Attempts = attempts
		return lastDispatched.execResult
	}
	// Nothing was ever dispatched, so no provider was asked and none can be
	// blamed. Every attempt died in local preparation — an undecryptable or
	// absent credential, an unusable adapter, a target that would not resolve
	// — and reporting that as an exhausted upstream is the same misattribution
	// the branch above exists to prevent, just one step earlier. It sends
	// operators to a provider status page for a fault that lives in our own
	// configuration, and it hands SDKs a retryable class for a condition that
	// no retry can clear.
	for i := range attempts {
		if attempts[i].Dispatched {
			return &ExecutionResult{Error: ErrAllTargetsExhausted, Attempts: attempts}
		}
	}
	return &ExecutionResult{Error: ErrNoTargetDispatchable, Attempts: attempts}
}

// attemptOutcome captures one call attempt and the executor's classification.
