package adapters

import (
	"context"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestRegisterTier1AdapterNormalizers_RegistersAll verifies that the
// type-assert loop in RegisterTier1AdapterNormalizers picks up every
// adapter that implements normalize.Normalizer (the consumer-web / IDE
// per-host adapters; standard-API vendor adapters carry no Normalize
// method). This test pins that contract so adding a Normalize method
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
	// loop. Standard-API vendor adapters — anthropic, gemini, bedrock,
	// vertex, deepseek, groq, … — carry no Normalize method and are NOT
	// registered as per-host Tier 1 entries: the shared codecs own
	// those wire-format keys via RegisterDefaultAIBuiltins.
	mustHave := []string{
		"chatgpt-web",           // consumer-surface adapter
		"claude-web",            // consumer-surface
		"chatgpt-web",           // dedupe ok
		"anthropic-console-web", // Anthropic web console, distinct from "anthropic"
		"gemini-web",            // distinct from AI builtin "gemini"
		// "openai-compat" is absent: the shared OpenAIChatNormalizer
		// owns that key (alias) via RegisterDefaultAIBuiltins — same
		// contract as anthropic / gemini above.
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
			t.Errorf("adapter %q overrode an AI-builtin normalizer — standard-API vendor adapters must not implement Normalize", k)
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
	// "moonshot" wire-format key belongs to the shared OpenAI-compatible
	// codec; the -web variant has a unique per-host ID and IS registered
	// here.
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
// (the bedrock / anthropic wire-format keys belong to the shared
// AnthropicMessagesNormalizer codec; only the -web per-host adapter
// registers here).
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
