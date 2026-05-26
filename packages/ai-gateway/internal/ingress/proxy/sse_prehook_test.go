package proxy

import (
	"context"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	sharednormalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
)

func TestBuildStreamPreHookCallback_NilDeps_ReturnsNil(t *testing.T) {
	if cb := buildStreamPreHookCallback(context.Background(), nil, "openai", "/v1/chat/completions", ""); cb != nil {
		t.Errorf("nil deps must produce nil callback; got non-nil")
	}
}

func TestBuildStreamPreHookCallback_NilRegistry_ReturnsNil(t *testing.T) {
	// Deps with NormalizeRegistry left unset (the dev-config case).
	deps := &Deps{}
	if cb := buildStreamPreHookCallback(context.Background(), deps, "openai", "/v1/chat/completions", ""); cb != nil {
		t.Errorf("nil NormalizeRegistry must produce nil callback")
	}
}

func TestBuildStreamPreHookCallback_StampsNormalizedOnCi(t *testing.T) {
	deps := &Deps{NormalizeRegistry: sharednormalize.BuildRegistry()}
	cb := buildStreamPreHookCallback(context.Background(), deps, "openai", "/v1/chat/completions", "text/event-stream")
	if cb == nil {
		t.Fatal("expected non-nil callback when registry is wired")
	}
	// Real OpenAI SSE delta — Registry tier 1 / 2 will claim it.
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	ci := &hookcore.HookInput{}
	cb(body, ci)
	if ci.Normalized == nil {
		t.Errorf("expected Normalized to be stamped on the HookInput; got nil")
	}
}

func TestBuildStreamPreHookCallback_NilBody_NoOp(t *testing.T) {
	deps := &Deps{NormalizeRegistry: sharednormalize.BuildRegistry()}
	cb := buildStreamPreHookCallback(context.Background(), deps, "openai", "/v1/chat/completions", "")
	if cb == nil {
		t.Fatal("expected non-nil callback")
	}
	ci := &hookcore.HookInput{}
	cb(nil, ci)
	if ci.Normalized != nil {
		t.Errorf("nil body should not stamp Normalized")
	}
	cb([]byte("non-empty"), nil)
	// nil ci must not panic — only assertion is "didn't panic".
}

func TestBuildStreamPreHookCallback_DefaultsContentType(t *testing.T) {
	deps := &Deps{NormalizeRegistry: sharednormalize.BuildRegistry()}
	// Pass empty acceptHeader — function must default to text/event-stream
	// internally so the Registry routing knows this is a streaming response.
	cb := buildStreamPreHookCallback(context.Background(), deps, "openai", "/v1/chat/completions", "")
	if cb == nil {
		t.Fatal("expected non-nil callback")
	}
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	ci := &hookcore.HookInput{}
	cb(body, ci)
	if ci.Normalized == nil {
		t.Errorf("expected Normalized to be stamped even with empty Accept header")
	}
}
