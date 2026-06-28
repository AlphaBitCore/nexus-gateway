package tlsbump

import (
	"context"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/responseprehook"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// buildSSEPreHookCallback returns a streaming.PreHookCallback that
// runs the supplied raw SSE body through the Registry's Tier 1+2+3
// normalize chain, then stamps both:
//
//	(1) checkpointInput.Normalized — so the hook executor sees rich
//	    structured chat content (model name, tool_calls, reasoning
//	    segments) instead of buildCheckpointInput's flat-text fallback
//	(2) auditInfo.ResponseNormalized — so the audit row's
//	    normalized_response column lands populated even on the
//	    streaming path
//
// The implementation delegates to shared
// transport/normalize/responseprehook.Build so all three ingress
// services (agent / compliance-proxy / ai-gateway) wire the same
// PreHookCallback shape. The audit-row stamp (#2) is service-specific
// and rides through the OnPayload option.
//
// Used by BOTH BufferPipeline (fires once between read + hooks) and
// LivePipeline (fires at every checkpoint with cumulative body bytes).
// Best-effort: nil body / nil registry / Normalize hard error are
// silently dropped — never abort hook execution because normalize
// stumbled.
func buildSSEPreHookCallback(
	ctx context.Context,
	bo *bumpOptions,
	audCtx *requestAuditCtx,
	auditInfo *compliance.AuditInfo,
	respInput *core.HookInput,
	respContentType string,
) streaming.PreHookCallback {
	if bo.normalizeRegistry == nil {
		return nil
	}
	adapterID := ""
	if audCtx != nil && audCtx.adapter != nil {
		adapterID = audCtx.adapter.ID()
	}
	endpointPath := ""
	if respInput != nil {
		endpointPath = respInput.Path
	}
	return responseprehook.Build(responseprehook.Options{
		Ctx:          ctx,
		Registry:     bo.normalizeRegistry,
		AdapterID:    adapterID,
		EndpointPath: endpointPath,
		ContentType:  respContentType,
		Direction:    normalize.DirectionResponse,
		OnPayload: func(payload *normalize.NormalizedPayload, _ []byte) {
			if auditInfo == nil || payload == nil {
				return
			}
			if b, err := json.Marshal(payload); err == nil {
				auditInfo.ResponseNormalized = b
			}
		},
	})
}
