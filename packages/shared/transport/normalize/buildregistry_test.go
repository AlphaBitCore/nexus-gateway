package normalize

import (
	"context"
	"os"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// #67 — every data-plane service must wire the same Tier 1+2+3
// Registry via BuildRegistry. These tests pin the contract:
//
//   - Returned Registry is non-nil and frozen (mutation panics).
//   - At least one Tier 1 AI builtin (openai-chat alias keyed under
//     "openai" + "openai-compat") resolves so per-provider normalize
//     hits the right codec instead of falling through to Tier 3
//     generic-http.
//   - At least one Tier 1 per-host adapter (chatgpt-web) resolves so
//     the agent / cp registry wiring isn't silently degraded.
//   - Tier 2 fallback (pattern probe) is installed so an unknown
//     adapterType plus a recognizable shape still produces an
//     ai-chat / ai-completion payload.

func TestBuildRegistry_Frozen_NonNil(t *testing.T) {
	reg := BuildRegistry()
	if reg == nil {
		t.Fatal("BuildRegistry returned nil")
	}
	// Frozen registry panics on Register.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on Register after Freeze")
		}
	}()
	reg.Register("test", nil)
}

func TestBuildRegistry_HasOpenAIChat(t *testing.T) {
	reg := BuildRegistry()
	// "openai" is keyed by RegisterDefaultAIBuiltins. Resolve via a
	// real Meta — that exercises the Coordinator's lookup chain
	// instead of poking the entries map directly.
	n := reg.Resolve(core.Meta{AdapterType: "openai", Direction: core.DirectionRequest, EndpointPath: "/v1/chat/completions"})
	if n == nil {
		t.Fatal("openai chat normalizer not resolvable — Tier 1 AI builtins not registered")
	}
}

func TestBuildRegistry_HasOpenAICompatAlias(t *testing.T) {
	// #72 — openai.Adapter.ID() returns "openai-compat"; without this
	// alias the agent's Tier 1 lookup miss falls all the way to Tier 3
	// generic-http and audit rows persist as http-text instead of
	// ai-chat. This test guards the alias from accidental removal.
	reg := BuildRegistry()
	n := reg.Resolve(core.Meta{AdapterType: "openai-compat", Direction: core.DirectionRequest, EndpointPath: "/v1/chat/completions"})
	if n == nil {
		t.Fatal("openai-compat alias missing — agent + cp will silently degrade to Tier 3")
	}
}

func TestBuildRegistry_HasChatGPTWebAdapter(t *testing.T) {
	reg := BuildRegistry()
	n := reg.Resolve(core.Meta{AdapterType: "chatgpt-web", Direction: core.DirectionRequest})
	if n == nil {
		t.Fatal("chatgpt-web Tier 1 adapter not registered")
	}
}

func TestBuildRegistry_SniffsKeyMissedAnthropicSSE(t *testing.T) {
	// Capture-side events routinely carry a host or tool name in
	// AdapterType and no endpoint path, so no Tier-1 key resolves. The
	// Tier-1.5 sniff pass must land such a body on the anthropic codec
	// through the FULL production assembly — same full-fidelity decode
	// as keyed traffic, not a Tier-3 verbatim dump. The wire is the
	// key-missed conformance corpus case (a real captured tool_use
	// stream with adapter/path stripped).
	wire, err := os.ReadFile("conformance/corpus/anthropic-sse-tooluse-keymissed/wire")
	if err != nil {
		t.Fatalf("read corpus wire: %v", err)
	}
	reg := BuildRegistry()
	p, err := reg.Normalize(context.Background(), wire, core.Meta{
		Direction: core.DirectionResponse,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Kind != core.KindAIChat {
		t.Fatalf("kind = %q, want %q (sniff pass not wired)", p.Kind, core.KindAIChat)
	}
	if p.DetectedSpec != "anthropic-messages" {
		t.Fatalf("detectedSpec = %q, want anthropic-messages", p.DetectedSpec)
	}
	if p.Model != "claude-opus-4-7" {
		t.Fatalf("model = %q, want claude-opus-4-7 from the wire's message_start", p.Model)
	}
	if len(p.Messages) == 0 || p.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("expected the folded assistant turn, got %+v", p.Messages)
	}
	if p.Usage == nil || p.Usage.CompletionTokens == nil || *p.Usage.CompletionTokens != 91 {
		t.Fatalf("expected usage folded from message_delta (completionTokens=91), got %+v", p.Usage)
	}
}

func TestBuildRegistry_SniffsKeyMissedResponsesAPI(t *testing.T) {
	// Key-missed capture of an OpenAI Responses-API body (AdapterType is
	// a host, no endpoint path) must land on the openai-responses codec
	// via the Tier-1.5 sniff — not on openai-chat (different wire under
	// the same vendor) and not on a Tier-2/3 projection.
	body := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-4o",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi!"}]}],` +
		`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	reg := BuildRegistry()
	p, err := reg.Normalize(context.Background(), []byte(body), core.Meta{
		AdapterType: "api.openai.com",
		Direction:   core.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Protocol != "openai-responses" || p.Kind != core.KindAIChat {
		t.Fatalf("protocol=%q kind=%q, want openai-responses ai-chat (sniff claim)", p.Protocol, p.Kind)
	}
	if len(p.Messages) != 1 || len(p.Messages[0].Content) != 1 || p.Messages[0].Content[0].Text != "Hi!" {
		t.Errorf("assistant message lost in sniff claim: %+v", p.Messages)
	}
	if p.Usage == nil || p.Usage.TotalTokens == nil || *p.Usage.TotalTokens != 5 {
		t.Errorf("usage lost in sniff claim: %+v", p.Usage)
	}
}

func TestBuildRegistry_TruncatedSniffMatchEndsAtTier3(t *testing.T) {
	// A truncated capture that PASSES a sniffer's LooksLike probe (the
	// Gemini markers sit in the head) but fails the codec's JSON decode
	// must still end at a Tier-3 structural projection with no error —
	// the sniff walk demotes the codec's hard parse error to soft
	// fall-through instead of failing the whole audit row.
	truncated := `{"candidates":[{"content":{"parts":[{"text":"Hel`
	reg := BuildRegistry()
	p, err := reg.Normalize(context.Background(), []byte(truncated), core.Meta{
		AdapterType: "generativelanguage.googleapis.com", // key-missed: host, not a wire-format key
		Direction:   core.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("truncated sniff-matched body must not error: %v", err)
	}
	if p.Kind != core.KindHTTPText {
		t.Errorf("kind = %q, want %q (Tier-3 structural projection)", p.Kind, core.KindHTTPText)
	}
	if p.Protocol != "generic-http" {
		t.Errorf("protocol = %q, want generic-http", p.Protocol)
	}
	if p.HTTP == nil || p.HTTP.BodyView == nil || p.HTTP.BodyView.Text != truncated {
		t.Errorf("Tier-3 projection must preserve the raw bytes verbatim; got %+v", p.HTTP)
	}
}

func TestBuildRegistry_NormalizeFallbackThroughTier3(t *testing.T) {
	// An unknown adapter + non-AI body should land on Tier 3
	// (generic-http) and produce an http-text payload — proves the
	// fallthrough is wired, not just Tier 1.
	reg := BuildRegistry()
	p, err := reg.Normalize(context.Background(), []byte(`<html><body>hi</body></html>`), core.Meta{
		ContentType: "text/html",
		Direction:   core.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Kind == "" {
		t.Error("Tier 3 fallback produced empty Kind — chain isn't wired")
	}
}
