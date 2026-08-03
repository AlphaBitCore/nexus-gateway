package executor

// result.go holds the shape the executor reports back: one Attempt per target it
// engaged, and the aggregate ExecutionResult the proxy handler reads. Kept apart
// from the execution loop because callers depend on this shape and not on how the
// loop arrives at it.

import (
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// Attempt records the outcome of a single upstream call.
type Attempt struct {
	Target         routingcore.RoutingTarget
	CredentialID   string
	CredentialName string
	StatusCode     int
	Error          string
	LatencyMs      int
	// Dispatched reports whether this entry reached the provider adapter.
	// False for a target that failed before any call left the process
	// (credential resolve, unusable adapter, request translation): those
	// entries carry a zero StatusCode, an empty Code and an empty
	// CredentialID, so reading one as the final upstream outcome reports a
	// rate-limited provider as an unavailable one and loses the credential
	// attribution. Use [ExecutionResult.Terminal] rather than testing this
	// field at a call site.
	Dispatched bool
	// Code is the canonical provider error code this attempt was classified
	// as ([provcore.CodeRateLimited], [provcore.CodeAuthFailed], …). Empty
	// on success, on a transport failure that produced no provider
	// envelope, and on every entry with Dispatched false.
	Code string
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
	Coerced []string
	// Truncated propagates provcore.Response.Truncated: the non-streaming
	// upstream body was clamped at the read cap (or decompressed-size bound)
	// before usage extraction, so the parsed token counts are incomplete. The
	// handler stamps usage_extraction_status="truncated" instead of "ok".
	Truncated bool
	Target    routingcore.RoutingTarget
	Attempts  []Attempt
	Error     error
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

// Terminal returns the last attempt that actually reached a provider — the
// one whose outcome decided this result — or nil when no target ever
// dispatched (every one failed to resolve, produced no usable adapter, or
// failed request translation).
//
// Callers must read the terminal upstream facts (canonical Code, upstream
// StatusCode, credential attribution) from here and never from the last
// element of Attempts. Attempts interleaves real calls with pre-dispatch
// failures, and a pre-dispatch failure landing last is not exotic: a
// rate-limited attempt opens the credential's circuit, which makes the
// following re-resolve fail, which appends an entry carrying a zero
// StatusCode — so the 429 that triggered the sequence is exactly the fact
// its own consequence would erase.
func (r *ExecutionResult) Terminal() *Attempt {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Dispatched {
			return &r.Attempts[i]
		}
	}
	return nil
}

// UpstreamAttempts counts the calls that actually reached a provider.
// Entries recording a pre-dispatch failure are excluded: they never left
// the process, so counting them claims an upstream call that never happened.
func (r *ExecutionResult) UpstreamAttempts() int {
	n := 0
	for i := range r.Attempts {
		if r.Attempts[i].Dispatched {
			n++
		}
	}
	return n
}
