package adapters

import (
	"context"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestRegisterTier1AdapterNormalizers_RegistersAll verifies that the
// type-assert loop in RegisterTier1AdapterNormalizers picks up every
// adapter that implements normalize.Normalizer. The expected count is
// "every built-in adapter except those covered by RegisterDefaultAIBuiltins
// (anthropic/gemini) and except cursor (gRPC-protobuf, intentionally
// skipped)". This test pins that contract so adding a Normalize method
// to a new adapter is automatically picked up without a separate
// registration step.
func TestRegisterTier1AdapterNormalizers_RegistersAll(t *testing.T) {
	reg := normalize.NewRegistry()
	RegisterTier1AdapterNormalizers(reg)

	gotKeys := reg.All()
	if len(gotKeys) == 0 {
		t.Fatal("expected at least one adapter Normalizer registered")
	}

	// Spot-check a few adapter IDs we know register via the type-assert
	// loop. Skipped (alreadyCoveredByAIBuiltins) adapters — anthropic,
	// gemini, bedrock, vertex, deepseek, groq, …  — are explicitly NOT
	// registered as per-host Tier 1 entries because the AI-builtin
	// normalizers (which run in the same Registry under those keys)
	// parse those formats more precisely.
	mustHave := []string{
		"chatgpt-web",           // consumer-surface adapter
		"claude-web",            // consumer-surface
		"chatgpt-web",           // dedupe ok
		"anthropic-console-web", // Anthropic web console, distinct from "anthropic"
		"gemini-web",            // distinct from AI builtin "gemini"
		// "openai-compat" was removed (#72): the AI-builtin
		// OpenAIChatNormalizer is now registered under that key too
		// (alias), so RegisterTier1AdapterNormalizers skips it via
		// alreadyCoveredByAIBuiltins — same contract as anthropic /
		// gemini above.
	}
	for _, id := range mustHave {
		found := false
		for _, k := range gotKeys {
			if k == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected adapter %q to be registered as Tier-1 Normalizer; got keys: %v", id, gotKeys)
		}
	}

	// Spot-check we did NOT register over AI-builtin keys.
	for _, k := range gotKeys {
		if k == "anthropic" || k == "gemini" {
			t.Errorf("adapter %q overrode an AI-builtin normalizer — should be skipped per alreadyCoveredByAIBuiltins", k)
		}
	}
}

// TestTier1AdapterNormalizes_OpenAIChatBody exercises a real OpenAI
// Chat request body through one of the bulk-registered adapters
// (deepseek). The adapter's Normalize is a thin wrapper that calls
// extract.NormalizeForAdapter; this test verifies the wiring end-to-end.
func TestTier1AdapterNormalizes_OpenAIChatBody(t *testing.T) {
	reg := normalize.NewRegistry()
	RegisterTier1AdapterNormalizers(reg)
	reg.Register("*:*:*", &noopNormalizer{}) // Tier 3 stub to satisfy Coordinator
	reg.Freeze()

	body := []byte(`{
		"model": "kimi-k2",
		"messages": [{"role": "user", "content": "hi"}],
		"temperature": 0.7
	}`)
	// Use "kimi-web" — Moonshot's consumer web variant. The API-level
	// "moonshot" adapter ID collides with the OpenAI-compatible AI
	// builtin (see alreadyCoveredByAIBuiltins) and is intentionally
	// skipped by Tier-1 per-host registration; the -web variant has a
	// unique ID and IS registered.
	payload, err := reg.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "kimi-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("kind: %v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "kimi-web" {
		t.Errorf("DetectedSpec: %q want kimi-web", payload.DetectedSpec)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("confidence: %v want >= 0.5", payload.Confidence)
	}
	if len(payload.Messages) != 1 || !strings.Contains(payload.Messages[0].Content[0].Text, "hi") {
		t.Errorf("messages: %+v", payload.Messages)
	}
}

// TestTier1AdapterNormalizes_AnthropicBody verifies an Anthropic-shape
// body routes through the anthropic-console-web adapter Normalizer
// (bedrock / anthropic adapter IDs collide with the AI-builtin
// AnthropicMessagesNormalizer and are intentionally skipped — see
// alreadyCoveredByAIBuiltins).
func TestTier1AdapterNormalizes_AnthropicBody(t *testing.T) {
	reg := normalize.NewRegistry()
	RegisterTier1AdapterNormalizers(reg)
	reg.Register("*:*:*", &noopNormalizer{})
	reg.Freeze()

	body := []byte(`{
		"model": "claude-haiku-4-5",
		"max_tokens": 256,
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi anthropic"}]}]
	}`)
	payload, err := reg.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "anthropic-console-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v", payload.Kind)
	}
	if payload.DetectedSpec != "anthropic-console-web" {
		t.Errorf("DetectedSpec: %q want anthropic-console-web", payload.DetectedSpec)
	}
}

// noopNormalizer is a Tier-3 placeholder so the Coordinator's chain
// has a terminal entry during these tests (the real generic-http
// catch-all isn't registered here because we skip RegisterDefaultAIBuiltins).
type noopNormalizer struct{}

func (n *noopNormalizer) ID() string { return "noop" }
func (n *noopNormalizer) Normalize(_ context.Context, raw []byte, _ normalize.Meta) (normalize.NormalizedPayload, error) {
	return normalize.NormalizedPayload{
		Kind:             normalize.KindHTTPText,
		NormalizeVersion: normalize.SchemaVersion,
		Confidence:       1.0,
	}, nil
}
