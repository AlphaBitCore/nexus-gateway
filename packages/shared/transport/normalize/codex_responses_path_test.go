package normalize

import (
	"context"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestBuildRegistry_CodexResponsesPath pins that codex traffic — the OpenAI
// Responses API posted to chatgpt.com/backend-api/codex/responses rather than
// /v1/responses — routes to the openai-responses codec and yields structured
// ai-chat messages, instead of falling through to Tier-3 generic-http.
// Regression guard for a real on-host capture that landed as http-json because
// the codex path was unregistered (the host's chatgpt-web adapter does not
// claim a Responses body). The body shape mirrors the real capture (model +
// instructions + input[] with role/content), content synthesized.
func TestBuildRegistry_CodexResponsesPath(t *testing.T) {
	body := `{"model":"gpt-5.4","instructions":"You are a coding agent.",` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"refactor this"}]}],` +
		`"tools":[],"reasoning":{"effort":"high"},"stream":true,"include":["reasoning.encrypted_content"]}`

	reg := BuildRegistry()
	// Route exactly as production does: the chatgpt.com host adapter + the codex
	// path. The path-only registration must win regardless of the resolved
	// adapter, so assert across the adapter values codex traffic can carry.
	for _, adapter := range []string{"chatgpt-web", "chatgpt.com", ""} {
		p, err := reg.Normalize(context.Background(), []byte(body), core.Meta{
			AdapterType:  adapter,
			EndpointPath: "/backend-api/codex/responses",
			Direction:    core.DirectionRequest,
		})
		if err != nil {
			t.Fatalf("adapter=%q: Normalize: %v", adapter, err)
		}
		if p.Protocol != "openai-responses" || p.Kind != core.KindAIChat {
			t.Fatalf("adapter=%q: protocol=%q kind=%q, want openai-responses ai-chat", adapter, p.Protocol, p.Kind)
		}
		if p.Model != "gpt-5.4" {
			t.Errorf("adapter=%q: model=%q, want gpt-5.4", adapter, p.Model)
		}
		if len(p.Messages) == 0 {
			t.Errorf("adapter=%q: no messages extracted from the Responses request", adapter)
		}
	}
}
