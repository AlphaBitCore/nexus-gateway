// stream_hooks.go — the response-hooks stage of the streaming stage
// chain: builds the per-checkpoint compliance pipeline runner and
// decides whether assistant deltas are held back until the first
// checkpoint approves. Owns streamState.hookRunner / holdBack.
package proxy

import (
	"context"
	"time"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// streamHooksStage resolves the response-stage hook wiring for the stream.
type streamHooksStage struct{ s *streamState }

func (st streamHooksStage) run() bool {
	s := st.s
	h := s.h
	r := s.r
	logger := s.logger

	// Derive endpoint type for hook filtering. The ingress descriptor is
	// stored on the request context by ServeProxy before any cache path
	// is entered; fall back to an empty type when not present.
	var streamEpType hookcore.EndpointType
	if streamIngress, ok := IngressFromContext(r.Context()); ok {
		streamEpType = typology.KindFromWireShape(streamIngress.WireShape)
	}
	streamModalities := []hookcore.Modality{hookcore.ModalityText}

	hookRunner := func(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		input.EndpointType = streamEpType
		input.OutputModality = streamModalities
		pipeline, err := h.deps.HookConfigCache.Resolver(ctx).BuildPipeline(
			"response", "AI_GATEWAY",
			streamEpType,
			streamModalities,
			5*time.Second, 15*time.Second, false, true /* strictFailClosed: reverse proxy refuses fail-closed-unbuildable */, logger,
		)
		if err != nil {
			// A build error here means a fail-closed response hook could not be
			// built (strictFailClosed=true). Refusing matches the non-stream
			// response path's 500 and the fail-closed intent — never silently
			// Approve a mandatory enforcer that is missing. Headers are already
			// sent for the SSE stream, so the in-band refusal is RejectHard
			// (the stream pipeline blocks/terminates content) rather than a 500.
			logger.Error("failed to build response hook pipeline for stream; refusing", "error", err)
			return &hookcore.CompliancePipelineResult{
				Decision:   hookcore.RejectHard,
				Reason:     "compliance hook pipeline build error",
				ReasonCode: "hook_pipeline_error",
			}
		}
		if pipeline == nil {
			return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
		}
		pipeline.SetAllowModify(true)
		pipeline.SetClearSoftOnApprove(true)
		return pipeline.Execute(ctx, input)
	}

	// HoldBack accumulates assistant deltas server-side until the first
	// compliance checkpoint approves. With FirstInspectChars=400 a
	// short response (e.g. Claude Code's "say hi" → ~5 tokens) never
	// hits the checkpoint mid-stream, so every chunk waits for the
	// final flush at end-of-stream — and the client sees a buffered
	// (Content-Length-bounded) body instead of a real SSE stream,
	// breaking Anthropic SDK / Claude Code's streaming UI rendering.
	//
	// Trade-off: HoldBack is ONLY useful when a response-stage hook
	// pipeline can actually reject content. If the response stage has
	// no rules wired (BuildPipeline returns nil), there is nothing to
	// gate on — we should pass chunks through live. Probe the resolver
	// once at stream entry; if the pipeline is nil we drop HoldBack so
	// the client sees real-time deltas. If a rule pack is configured
	// later, the next request rebuilds and re-enters HoldBack.
	holdBack := true
	// Conservative default: assume a response hook may run, so the live
	// pipeline installs the Registry-normalize PreHook. Lowered to false only
	// when the probe proves there are no response-stage rules.
	responseHooksActive := true
	if h.deps != nil && h.deps.HookConfigCache != nil {
		probe, probeErr := h.deps.HookConfigCache.Resolver(r.Context()).BuildPipeline(
			"response", "AI_GATEWAY",
			streamEpType,
			streamModalities,
			5*time.Second, 15*time.Second, false, true /* strictFailClosed: reverse proxy refuses fail-closed-unbuildable */, logger,
		)
		// Best-effort probe: decides whether to drop HoldBack for a snappier UI
		// AND whether to skip the PreHook normalize, when no response-stage rules
		// exist. On a build error (probeErr != nil) we deliberately keep
		// holdBack=true and responseHooksActive=true — the actual refusal is
		// enforced by hookRunner above when the stream pipeline is built
		// per-checkpoint; the probe must not Approve-or-refuse on its own.
		if probeErr == nil && probe == nil {
			holdBack = false
			responseHooksActive = false
		}
	}

	s.hookRunner = hookRunner
	s.holdBack = holdBack
	s.responseHooksActive = responseHooksActive
	return true
}
