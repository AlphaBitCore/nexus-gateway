// stream_hooks.go — the response-hooks stage of the streaming stage
// chain: builds the per-checkpoint compliance pipeline runner and the
// scope-derived enforcement posture (block/redact) that routes the stream.
// Owns streamState.hookRunner.
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

	// Probe the response-stage resolver once at stream entry to decide whether the
	// live pipeline installs the Registry-normalize PreHook: assume a response hook
	// may run (so hooks see structured chat content) and lower to false only when
	// the probe proves there are no response-stage rules. The live path is
	// audit-only (B1) — it never holds back or rewrites deltas — so this probe no
	// longer gates delivery; it only skips the normalize cost on rule-free streams.
	responseHooksActive := true
	var enforcingBlock, enforcingRedact bool
	if h.deps != nil && h.deps.HookConfigCache != nil {
		probe, probeErr := h.deps.HookConfigCache.Resolver(r.Context()).BuildPipeline(
			"response", "AI_GATEWAY",
			streamEpType,
			streamModalities,
			5*time.Second, 15*time.Second, false, true /* strictFailClosed: reverse proxy refuses fail-closed-unbuildable */, logger,
		)
		// The probe decides whether to skip the PreHook normalize when no
		// response-stage rules exist, plus the scope-derived enforcement posture
		// that stream_shape.go reads to pick the streaming mode upfront.
		switch {
		case probeErr == nil && probe == nil:
			responseHooksActive = false
		case probeErr == nil && probe != nil:
			// Scope-derived enforcement posture is content-independent (it reads the
			// bound hooks' onMatch actions, not the body), so it is safe to read off
			// the probe here.
			enforcingBlock = probe.MayBlock()
			enforcingRedact = probe.MayRedact()
		default:
			// probeErr != nil → a FAIL-CLOSED response hook is unbuildable (the only
			// condition under which BuildPipeline errors). The live path is audit-only
			// and can no longer enforce in-stream, so force the request to BUFFER
			// (enforcingBlock=true): redactCanonicalBuffer re-runs the build, hits the
			// same error, and fails closed with an in-band error frame. Without this a
			// fail-closed hook would silently fail OPEN on a live/passthrough stream.
			enforcingBlock = true
		}
	}

	s.hookRunner = hookRunner
	s.responseHooksActive = responseHooksActive
	s.responseEnforcingBlock = enforcingBlock
	s.responseEnforcingRedact = enforcingRedact
	return true
}
