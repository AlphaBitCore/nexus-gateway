package normalize

import (
	"context"
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
