package proxy

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
)

// TestFormatToTrafficAdapterID_Exhaustive enforces that every
// [provcore.Format] enum value resolves to a known registry ID. The
// test iterates [provcore.AllFormats] so adding a new Format without
// updating this switch fails loudly.
func TestFormatToTrafficAdapterID_Exhaustive(t *testing.T) {
	want := map[provcore.Format]string{
		provcore.FormatOpenAI:      "openai-compat",
		provcore.FormatDeepSeek:    "deepseek",
		provcore.FormatGLM:         "glm",
		provcore.FormatAzureOpenAI: "azure-openai",
		provcore.FormatAnthropic:   "anthropic",
		provcore.FormatGemini:      "gemini",
		provcore.FormatMiniMax:     "minimax",
		provcore.FormatBedrock:     "bedrock",
		provcore.FormatVertex:      "vertex",
		provcore.FormatCohere:      "cohere",
		provcore.FormatHuggingFace: "huggingface",
		provcore.FormatReplicate:   "replicate",
		provcore.FormatMistral:     "mistral",
		provcore.FormatXai:         "xai",
		provcore.FormatGroq:        "groq",
		provcore.FormatPerplexity:  "perplexity",
		provcore.FormatTogether:    "together",
		provcore.FormatFireworks:   "fireworks",
		provcore.FormatMoonshot:    "moonshot",
		provcore.FormatVoyage:      "voyage",
	}

	for _, f := range provcore.AllFormats() {
		got := formatToTrafficAdapterID(f)
		expected, ok := want[f]
		if !ok {
			t.Fatalf("new Format %q added without updating this test map", f)
		}
		if got != expected {
			t.Errorf("formatToTrafficAdapterID(%q) = %q, want %q", f, got, expected)
		}
	}

	if got := formatToTrafficAdapterID(provcore.Format("unknown")); got != "generic-jsonpath" {
		t.Errorf("unknown format should fall back to generic-jsonpath, got %q", got)
	}
}

// TestTrafficAdapterFor_RoutesByIngressFormat verifies that the format
// passed into trafficAdapterFor resolves to an adapter whose
// registry ID matches the plan's mapping, end-to-end against the real
// built-in registry. This is the s5 regression: before the fix every
// ingress used the hard-coded OpenAI adapter.
func TestTrafficAdapterFor_RoutesByIngressFormat(t *testing.T) {
	reg := traffic.NewAdapterRegistry("nexus_ai_gateway_test")
	adapters.RegisterBuiltins(reg)
	reg.Freeze()
	h := &Handler{deps: &Deps{TrafficAdapters: reg}}

	cases := []struct {
		format provcore.Format
		wantID string
	}{
		{provcore.FormatOpenAI, "openai-compat"},
		{provcore.FormatAnthropic, "anthropic"},
		{provcore.FormatGemini, "gemini"},
		{provcore.FormatGLM, "glm"},
		{provcore.FormatMiniMax, "minimax"},
		{provcore.FormatAzureOpenAI, "azure-openai"},
		{provcore.FormatDeepSeek, "deepseek"},
		{provcore.FormatBedrock, "bedrock"},
		{provcore.FormatVertex, "vertex"},
		{provcore.FormatMistral, "mistral"},
		{provcore.FormatXai, "xai"},
		{provcore.FormatGroq, "groq"},
		{provcore.FormatPerplexity, "perplexity"},
		{provcore.FormatTogether, "together"},
		{provcore.FormatFireworks, "fireworks"},
		{provcore.FormatMoonshot, "moonshot"},
		{provcore.FormatVoyage, "voyage"},
	}
	for _, tc := range cases {
		t.Run(string(tc.format), func(t *testing.T) {
			got := h.trafficAdapterFor(tc.format)
			if got == nil {
				t.Fatalf("trafficAdapterFor(%q) returned nil", tc.format)
			}
			if got.ID() != tc.wantID {
				t.Errorf("trafficAdapterFor(%q).ID() = %q, want %q", tc.format, got.ID(), tc.wantID)
			}
		})
	}
}

// TestTrafficAdapterFor_NilDeps returns nil safely rather than panicking.
func TestTrafficAdapterFor_NilDeps(t *testing.T) {
	var h *Handler
	if got := h.trafficAdapterFor(provcore.FormatOpenAI); got != nil {
		t.Errorf("nil handler should yield nil adapter, got %v", got)
	}
	h2 := &Handler{}
	if got := h2.trafficAdapterFor(provcore.FormatOpenAI); got != nil {
		t.Errorf("handler with nil deps should yield nil adapter, got %v", got)
	}
}

// TestTrafficAdapterFor_SingleAdapterTestHatch verifies that tests
// can wire a single [Deps.TrafficAdapter] without standing up a full
// registry; production code must use TrafficAdapters.
func TestTrafficAdapterFor_SingleAdapterTestHatch(t *testing.T) {
	stub := &stubTrafficAdapter{id: "openai-compat"}
	h := &Handler{deps: &Deps{TrafficAdapter: stub}}
	if got := h.trafficAdapterFor(provcore.FormatAnthropic); got != stub {
		t.Errorf("expected single-adapter test hatch to return stub regardless of format; got %v", got)
	}
}

// TestTrafficAdapterFor_UnregisteredFallsBackToGeneric constructs a
// registry that omits a known format and asserts the helper falls back
// to `generic-jsonpath`. Protects against partial registry wiring.
func TestTrafficAdapterFor_UnregisteredFallsBackToGeneric(t *testing.T) {
	reg := traffic.NewAdapterRegistry("nexus_ai_gateway_test_partial")
	if err := reg.Register("generic-jsonpath", func() traffic.Adapter { return &stubTrafficAdapter{id: "generic-jsonpath"} }); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()
	h := &Handler{deps: &Deps{TrafficAdapters: reg}}

	got := h.trafficAdapterFor(provcore.FormatAnthropic)
	if got == nil {
		t.Fatal("expected fallback adapter, got nil")
	}
	if got.ID() != "generic-jsonpath" {
		t.Errorf("expected fallback to generic-jsonpath, got %q", got.ID())
	}
}
