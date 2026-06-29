package responseprehook_test

import (
	"context"
	"github.com/goccy/go-json"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/responseprehook"
)

// TestResponsePreHookBuilder_CallerShape_Equivalence is named to
// disambiguate from the true three-pipeline consistency test
// `TestThreePipelineConsistency` in
// `packages/ai-gateway/internal/platform/streaming/cross_pipeline_consistency_test.go`
// which exercises shared.BufferPipeline + shared.LivePipeline +
// ai-gateway.LivePipeline as actual pipeline implementations).
//
// This test asserts the narrower invariant: the responseprehook.Build
// HELPER produces equivalent ci.Normalized regardless of which call
// shape (tlsbump / ai-gateway) invoked it. It does NOT exercise three
// different pipeline implementations — both shapes here call the
// same builder, then the same shared Registry. The day someone forks
// the helper or sneaks a per-service shortcut in, this test fails.
//
// The two call-shapes under test mirror real callers:
//   - tlsbump shape: AdapterID = audCtx.adapter.ID() (e.g. "anthropic"
//     from the traffic-adapter registry); used by agent + compliance-
//     proxy. The OnPayload closure stamps audit info — that side
//     effect must NOT alter ci.Normalized.
//   - ai-gateway shape: AdapterID = target.AdapterType (e.g.
//     "anthropic" via provcore.FormatAnthropic); no OnPayload.
//
// Both call shapes feed the same shared Registry, so the resulting
// ci.Normalized must be bit-identical when serialized to JSON. The
// test serializes both and asserts equality — comparing whole
// structures defends against future field additions that one side
// might forget to mirror.
func TestResponsePreHookBuilder_CallerShape_Equivalence(t *testing.T) {
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)

	cases := []struct {
		name        string
		adapterID   string
		contentType string
		body        []byte
	}{
		{
			name:        "anthropic-messages_sse",
			adapterID:   "anthropic",
			contentType: "text/event-stream",
			body: []byte("event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-3-sonnet\",\"usage\":{\"input_tokens\":10}}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
				"event: message_stop\n" +
				"data: {\"type\":\"message_stop\"}\n\n"),
		},
		{
			name:        "openai-chat_sse",
			adapterID:   "openai",
			contentType: "text/event-stream",
			body: []byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: [DONE]\n\n"),
		},
		{
			name:        "gemini-generate_sse",
			adapterID:   "gemini",
			contentType: "text/event-stream",
			body:        []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"),
		},
	}

	for _, c := range cases {

		t.Run(c.name, func(t *testing.T) {
			// Tlsbump-shape call: same Adapter + path + content-type
			// as agent + compliance-proxy use, plus an OnPayload that
			// records the payload as the audit-stamp would. Mutating
			// auditInfo MUST NOT change ci.Normalized.
			var auditSeen []byte
			tlsbumpCB := responseprehook.Build(responseprehook.Options{
				Ctx:          context.Background(),
				Registry:     reg,
				AdapterID:    c.adapterID,
				EndpointPath: "",
				ContentType:  c.contentType,
				Direction:    normcore.DirectionResponse,
				OnPayload: func(payload *normcore.NormalizedPayload, _ []byte) {
					if b, err := json.Marshal(payload); err == nil {
						auditSeen = b
					}
				},
			})
			if tlsbumpCB == nil {
				t.Fatalf("tlsbump-shape Build returned nil callback")
			}

			// ai-gateway-shape call: no OnPayload. Same Registry + same
			// adapter + same content-type. The PreHookCallback MUST
			// stamp ci.Normalized to the SAME JSON shape as the
			// tlsbump-shape call.
			aigwCB := responseprehook.Build(responseprehook.Options{
				Ctx:          context.Background(),
				Registry:     reg,
				AdapterID:    c.adapterID,
				EndpointPath: "",
				ContentType:  c.contentType,
				Direction:    normcore.DirectionResponse,
			})
			if aigwCB == nil {
				t.Fatalf("ai-gateway-shape Build returned nil callback")
			}

			tlsbumpCI := &hookcore.HookInput{}
			aigwCI := &hookcore.HookInput{}
			tlsbumpCB(c.body, tlsbumpCI)
			aigwCB(c.body, aigwCI)

			if tlsbumpCI.Normalized == nil || aigwCI.Normalized == nil {
				t.Fatalf("expected both callbacks to stamp Normalized; tlsbump=%v aigw=%v", tlsbumpCI.Normalized, aigwCI.Normalized)
			}

			tlsbumpJSON, err := json.Marshal(tlsbumpCI.Normalized)
			if err != nil {
				t.Fatalf("marshal tlsbump payload: %v", err)
			}
			aigwJSON, err := json.Marshal(aigwCI.Normalized)
			if err != nil {
				t.Fatalf("marshal aigw payload: %v", err)
			}

			if string(tlsbumpJSON) != string(aigwJSON) {
				t.Fatalf("cross-service Normalized payload divergence:\n  tlsbump: %s\n  aigw:    %s", tlsbumpJSON, aigwJSON)
			}
			if len(auditSeen) == 0 {
				t.Errorf("tlsbump OnPayload didn't fire — audit-info stamp path broken")
			}
			if string(auditSeen) != string(tlsbumpJSON) {
				t.Errorf("tlsbump OnPayload saw a different payload than ci.Normalized:\n  audit: %s\n  ci:    %s", auditSeen, tlsbumpJSON)
			}
		})
	}
}
