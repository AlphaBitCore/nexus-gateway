// Forward-header metric callback. The providers package emits
// counter increments via this function pointer so it does not have to
// import the metrics package (which depends on shared/opsmetrics
// and would create a wider dependency surface for every spec_*
// subpackage's tests). cmd/ai-gateway/main.go wires the real
// implementation at startup; tests leave it at the no-op default.
//
// Mirrors the executor.SetMetricsRecorder pattern.

package dispatch

import "sync/atomic"

// ForwardHeaderDropFunc is the signature for the dropped-header
// counter increment. Direction is "request" or "response";
// adapterType is the Format slug (e.g. "openai", "anthropic");
// headerLabel is the bucketed header label
// (forwardheader.BucketDroppedHeader output).
type ForwardHeaderDropFunc func(direction, adapterType, headerLabel string)

// forwardHeaderDropFn is the active callback. atomic.Pointer keeps
// SetForwardHeaderDropFn safe to call concurrently with hot-path
// emit (in practice it is set once at startup).
var forwardHeaderDropFn atomic.Pointer[ForwardHeaderDropFunc]

// SetForwardHeaderDropFn installs fn as the active counter callback.
// Pass nil to reset to the no-op default.
func SetForwardHeaderDropFn(fn ForwardHeaderDropFunc) {
	if fn == nil {
		forwardHeaderDropFn.Store(nil)
		return
	}
	forwardHeaderDropFn.Store(&fn)
}

// emitForwardHeaderDrop is the hot-path entry point used by
// specAdapter.forwardHeaders / FilterResponseHeaders.
func emitForwardHeaderDrop(direction, adapterType, headerLabel string) {
	p := forwardHeaderDropFn.Load()
	if p == nil {
		return
	}
	(*p)(direction, adapterType, headerLabel)
}

// ReasoningPassthroughFunc is the signature for the
// nexus_aigw_reasoning_passthrough_total counter increment. Provider is
// the adapter slug (e.g. "anthropic", "gemini"); action is one of
// "injected" (extension found and forwarded to upstream) or
// "skipped_malformed" (extension found but had wrong JSON type and was
// dropped with a WarnOnce).
type ReasoningPassthroughFunc func(provider, action string)

var reasoningPassthroughFn atomic.Pointer[ReasoningPassthroughFunc]

// SetReasoningPassthroughFn installs fn as the active counter callback
// for nexus.ext.<provider>.<reasoning_key> passthrough events. Pass nil
// to reset to the no-op default. Wired in cmd/ai-gateway/main.go at
// startup; tests leave it nil so they don't depend on the metrics
// package.
func SetReasoningPassthroughFn(fn ReasoningPassthroughFunc) {
	if fn == nil {
		reasoningPassthroughFn.Store(nil)
		return
	}
	reasoningPassthroughFn.Store(&fn)
}

// EmitReasoningPassthrough is the hot-path entry point used by codecs
// (spec_anthropic, spec_gemini) when reading nexus.ext.*.thinking* into
// the outgoing upstream body. Exported because codec packages are
// siblings of this package, not children.
func EmitReasoningPassthrough(provider, action string) {
	p := reasoningPassthroughFn.Load()
	if p == nil {
		return
	}
	(*p)(provider, action)
}
