package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// attempt.go — per-attempt execution + response classification, split from
// executor.go under the file-size ratchet (same package, same TargetExecutor).

type attemptOutcome struct {
	attempt    Attempt
	execResult *ExecutionResult // populated on classSuccess and classNoFailoverNoRetry
	class      errClass
	errCl      cfgpolicy.ErrorClass // empty unless class is one of the retryable kinds
}

// translateAttempt builds a failover Attempt for a cross-format bridge
// translate failure — a gateway-side codec Fail, NOT an upstream response. It
// leaves the attempt Dispatched=false, so per the Attempt.Dispatched invariant
// it carries a zero StatusCode and empty Code (stamping the codec's own status
// here would contradict that load-bearing invariant, which future readers trust
// to know a non-dispatched attempt has no meaningful upstream status). The
// typed error's message is preserved in the Error string for the attempt log;
// the client-facing typed surfacing of a codec Fail happens on the
// prepare/dispatch path — spec_adapter Execute + the cache-prep writeCodecErr —
// which run before this failover leg, and a translate failure mid-failover
// never becomes the terminal (dispatched) outcome the client sees anyway.
func translateAttempt(target routingcore.RoutingTarget, terr error) Attempt {
	return Attempt{Target: target, Error: fmt.Sprintf("hub translate: %v", terr)}
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
func (e *TargetExecutor) attemptWithBody(ctx context.Context, adapter provcore.Adapter, req provcore.Request, target routingcore.RoutingTarget, body []byte, rewrites []string, urlOverride string) attemptOutcome {
	start := time.Now()
	resp, err := adapter.ExecuteWithBody(ctx, req, body, rewrites, urlOverride)
	return e.classifyAttempt(start, resp, err, target)
}

func (e *TargetExecutor) classifyAttempt(start time.Time, resp *provcore.Response, err error, target routingcore.RoutingTarget) attemptOutcome {
	latency := int(time.Since(start).Milliseconds())

	a := Attempt{
		Target:    target,
		LatencyMs: latency,
		// classifyAttempt runs only after the adapter returned, so every
		// entry it builds records a call that left the process.
		Dispatched: true,
	}
	// Recorded on every outcome, not only success: a coercion that preceded a
	// FAILED dispatch is the one an operator most wants to see, because "we
	// rewrote this field and then it 400ed" is the shape of the question.
	//
	// Guarded, because a TRANSPORT failure produces no response at all — the
	// adapter returns (nil, err), and reading through it here would panic on
	// every connection reset the gateway survives today.
	if resp != nil {
		a.Coerced = resp.Coerced
	}

	cls, errCl := classify(resp, err)
	a.ErrorClass = cls.name()

	// The canonical code the classifier branched on, kept on the attempt so
	// the terminal outcome carries its cause to the handler instead of being
	// re-derived there from the raw upstream status. The two are not
	// interchangeable: Status is whatever the provider replied with, while
	// Code is normalised from the provider's own error type, so a provider
	// that reports "overloaded" on a non-429 status still classifies as
	// rate-limited here.
	var pe *provcore.ProviderError
	if errors.As(err, &pe) {
		a.Code = pe.Code
	}

	switch {
	case cls == classSuccess:
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
				Truncated:    resp.Truncated,
				Target:       target,
				TargetMethod: resp.TargetMethod,
				TargetPath:   resp.TargetPath,
			},
		}
	case cls == classClientGone:
		// No recordHealth call, and that omission is the point. The provider
		// may have been about to answer perfectly; the only thing that failed
		// is our ability to deliver the answer. Counting it against the
		// provider lets a wave of client disconnects push every provider it
		// touched past the unavailability threshold, and the requests that
		// then get routed away are other people's.
		if pe != nil {
			a.StatusCode = pe.Status
			a.Error = pe.Error()
		}
		return attemptOutcome{
			attempt: a,
			class:   cls,
			execResult: &ExecutionResult{
				StatusCode: 499,
				Target:     target,
				Error:      context.Canceled,
			},
		}

	case cls.surfacesUpstreamEnvelope():
		// 4xx terminal (or context overflow on what may be the last
		// target) — surface the upstream body + headers directly so the
		// handler can either pass through (ingress == upstream) or
		// reshape the envelope for a cross-format client.
		if pe != nil {
			a.StatusCode = pe.Status
			a.Error = pe.Error()
			if cls.recordsProviderHealth() {
				e.recordHealth(target, false, latency)
			}
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
		if cls.recordsProviderHealth() {
			e.recordHealth(target, false, latency)
		}
		return attemptOutcome{
			attempt:    a,
			class:      cls,
			execResult: &ExecutionResult{StatusCode: http.StatusInternalServerError, Target: target},
		}
	default:
		// Retryable failure (network / timeout / 429 / 5xx).
		if pe != nil {
			a.StatusCode = pe.Status
			a.Error = pe.Error()
		} else if err != nil {
			a.Error = err.Error()
		}
		// Every class reaching here is a deprioritise class, all of which are
		// evidence about the provider — asked rather than assumed, so a class
		// routed here later cannot skip the question.
		if cls.recordsProviderHealth() {
			e.recordHealth(target, false, latency)
		}
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

// recordCredentialStats feeds the credential circuit breaker, which counts
// failures per key and opens the circuit when authentication failures reach a
// threshold.
//
// Two classes are withheld, for the same reason health tracking withholds them:
// they are not the credential's business, and the breaker acts on what it is
// told. A cancelled caller says nothing about the key that was about to be
// used. A local decode failure happened on our side of a response the upstream
// had already accepted — with that key, successfully.
//
// The breaker reads raw HTTP status codes and derives its own vocabulary from
// them, so it cannot make this distinction itself: a client-gone 499 reads as
// an ordinary 4xx and a local-processing 502 as an upstream fault. Filtering at
// the source is what keeps the two classifiers from disagreeing.
func (e *TargetExecutor) recordCredentialStats(credID string, o *attemptOutcome) {
	if e.stats == nil || credID == "" {
		return
	}
	if !o.class.chargesCredential() {
		return
	}
	e.stats.RecordAttempt(credID, o.attempt.StatusCode, o.attempt.Error)
}
