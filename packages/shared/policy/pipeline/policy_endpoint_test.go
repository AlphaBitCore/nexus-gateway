package pipeline

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestBuildPipeline_EmbeddingsIncludesTextClassAHooksOnRequest verifies
// that for an embeddings endpoint request, Class-A text hooks
// (keyword-filter, pii-detector) ARE included because embedding inputs are
// text and must be scanned. Class-B hooks (rate-limiter, request-size-validator)
// are also included.
func TestBuildPipeline_EmbeddingsDropsTextClassAHooks(t *testing.T) {
	logger := testLogger()
	registry := builtins.Registry.Clone()
	registry.Freeze()

	// Minimal valid configs for each hook category.
	classAConfigs := []core.HookConfig{
		{
			ID:               "kw-1",
			ImplementationID: "keyword-filter",
			Name:             "keyword",
			Stage:            "request",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"patterns": []any{
					map[string]any{"pattern": "secret", "category": "test"},
				},
				"onMatch": map[string]any{"inflightAction": "block-hard", "storageAction": "keep"},
			},
		},
		{
			ID:               "pii-1",
			ImplementationID: "pii-detector",
			Name:             "pii",
			Stage:            "request",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"patternDefinitions": []any{
					map[string]any{"id": "email", "regex": `\b[\w.+-]+@[\w-]+\.[a-z]{2,}\b`, "flags": "i"},
				},
				"onMatch": map[string]any{"inflightAction": "block-hard", "storageAction": "keep"},
			},
		},
	}

	classBConfigs := []core.HookConfig{
		{
			ID:               "rl-1",
			ImplementationID: "rate-limiter",
			Name:             "rate-limit",
			Stage:            "request",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"maxRequests":   float64(100),
				"windowSeconds": float64(60),
				"keyType":       "source_ip",
			},
		},
		{
			ID:               "rs-1",
			ImplementationID: "request-size-validator",
			Name:             "size",
			Stage:            "request",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"maxSizeBytes": float64(1048576),
			},
		},
	}

	allConfigs := make([]core.HookConfig, 0, len(classAConfigs)+len(classBConfigs))
	allConfigs = append(allConfigs, classAConfigs...)
	allConfigs = append(allConfigs, classBConfigs...)
	resolver := NewPolicyResolver(allConfigs, registry, logger)

	// Build pipeline for embeddings endpoint REQUEST with text modality.
	// Class-A text hooks MUST be present on the request side because
	// embedding inputs are plain text and must be inspected.
	pipe, err := resolver.BuildPipeline(
		"request", "AI_GATEWAY",
		core.EndpointTypeEmbeddings,
		[]core.Modality{core.ModalityText},
		5*time.Second, 30*time.Second,
		false,
		false,
		logger,
	)
	if err != nil {
		t.Fatalf("BuildPipeline error: %v", err)
	}
	if pipe == nil {
		t.Fatal("expected non-nil pipeline for embeddings request")
		return
	}

	// All 4 hooks (2 Class-A + 2 Class-B) must be present in the request pipeline.
	totalExpected := len(classAConfigs) + len(classBConfigs)
	if len(pipe.hooks) != totalExpected {
		names := make([]string, len(pipe.hooks))
		for i, bh := range pipe.hooks {
			names[i] = bh.config.ImplementationID
		}
		t.Errorf("expected %d hooks in embeddings request pipeline, got %d: %v", totalExpected, len(pipe.hooks), names)
	}

	// Verify Class-A hooks ARE present.
	classAIDs := map[string]bool{"keyword-filter": true, "pii-detector": true}
	classAFound := 0
	for _, bh := range pipe.hooks {
		if classAIDs[bh.config.ImplementationID] {
			classAFound++
		}
	}
	if classAFound != len(classAConfigs) {
		t.Errorf("expected %d Class-A hooks in embeddings request pipeline, found %d", len(classAConfigs), classAFound)
	}
}

// TestBuildPipeline_EmbeddingsResponseDropsTextClassAHooks verifies
// that for an embeddings endpoint RESPONSE, Class-A text hooks
// (keyword-filter, pii-detector — both implement TextOnlyContentScanningMarker)
// are gated out because embedding responses are float vectors, not text. Class-B
// hooks are still included.
func TestBuildPipeline_EmbeddingsResponseDropsTextClassAHooks(t *testing.T) {
	logger := testLogger()
	registry := builtins.Registry.Clone()
	registry.Freeze()

	allConfigs := []core.HookConfig{
		{
			ID:               "kw-resp",
			ImplementationID: "keyword-filter",
			Name:             "keyword",
			Stage:            "response",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"patterns": []any{
					map[string]any{"pattern": "secret", "category": "test"},
				},
				"onMatch": map[string]any{"inflightAction": "block-hard", "storageAction": "keep"},
			},
		},
		{
			ID:               "pii-resp",
			ImplementationID: "pii-detector",
			Name:             "pii",
			Stage:            "response",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"patternDefinitions": []any{
					map[string]any{"id": "email", "regex": `\b[\w.+-]+@[\w-]+\.[a-z]{2,}\b`, "flags": "i"},
				},
				"onMatch": map[string]any{"inflightAction": "block-hard", "storageAction": "keep"},
			},
		},
		{
			ID:               "rs-resp",
			ImplementationID: "request-size-validator",
			Name:             "size",
			Stage:            "response",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config:           map[string]any{"maxSizeBytes": float64(1048576)},
		},
	}
	resolver := NewPolicyResolver(allConfigs, registry, logger)

	// Build pipeline for embeddings endpoint RESPONSE.
	pipe, err := resolver.BuildPipeline(
		"response", "AI_GATEWAY",
		core.EndpointTypeEmbeddings,
		[]core.Modality{core.ModalityText},
		5*time.Second, 30*time.Second,
		false,
		false,
		logger,
	)
	if err != nil {
		t.Fatalf("BuildPipeline error: %v", err)
	}
	if pipe == nil {
		t.Fatal("expected non-nil pipeline (Class-B hook request-size-validator should be present)")
		return
	}

	// Only the Class-B hook (request-size-validator) should survive —
	// Class-A hooks are gated out by the embedding_response_no_text filter.
	classAIDs := map[string]bool{"keyword-filter": true, "pii-detector": true}
	for _, bh := range pipe.hooks {
		if classAIDs[bh.config.ImplementationID] {
			t.Errorf("Class-A hook %q must NOT appear in embeddings response pipeline (vectors are not text)", bh.config.ImplementationID)
		}
	}
	// Class-B hook must be present.
	classBFound := false
	for _, bh := range pipe.hooks {
		if bh.config.ImplementationID == "request-size-validator" {
			classBFound = true
		}
	}
	if !classBFound {
		t.Error("Class-B hook request-size-validator must appear in embeddings response pipeline")
	}
}

// TestBuildPipeline_ChatIncludesAllHooks verifies that a chat endpoint
// produces the full expected hook set (Class-A + Class-B).
func TestBuildPipeline_ChatIncludesClassAAndClassBHooks(t *testing.T) {
	logger := testLogger()
	registry := builtins.Registry.Clone()
	registry.Freeze()

	configs := []core.HookConfig{
		{
			ID:               "kw-chat",
			ImplementationID: "keyword-filter",
			Name:             "keyword",
			Stage:            "request",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"patterns": []any{
					map[string]any{"pattern": "secret", "category": "test"},
				},
				"onMatch": map[string]any{"inflightAction": "block-hard", "storageAction": "keep"},
			},
		},
		{
			ID:               "rl-chat",
			ImplementationID: "rate-limiter",
			Name:             "rate-limit",
			Stage:            "request",
			Enabled:          true,
			FailBehavior:     "fail-open",
			Config: map[string]any{
				"maxRequests":   float64(100),
				"windowSeconds": float64(60),
				"keyType":       "source_ip",
			},
		},
	}

	resolver := NewPolicyResolver(configs, registry, logger)
	pipe, err := resolver.BuildPipeline(
		"request", "AI_GATEWAY",
		core.EndpointTypeChat,
		[]core.Modality{core.ModalityText},
		5*time.Second, 30*time.Second,
		false,
		false,
		logger,
	)
	if err != nil {
		t.Fatalf("BuildPipeline error: %v", err)
	}
	if pipe == nil {
		t.Fatal("expected non-nil pipeline for chat endpoint")
		return
	}
	if len(pipe.hooks) != 2 {
		t.Errorf("expected 2 hooks for chat, got %d", len(pipe.hooks))
	}
}

// TestEndpointTypeFromPath verifies the path→EndpointType mapping helper.
func TestEndpointTypeFromPath(t *testing.T) {
	tests := []struct {
		path string
		want core.EndpointType
	}{
		{"chat/completions", core.EndpointTypeChat},
		{"completions", core.EndpointTypeChat},
		{"responses", core.EndpointTypeChat},
		{"embeddings", core.EndpointTypeEmbeddings},
		{"audio/transcriptions", core.EndpointTypeSTT},
		{"audio/translations", core.EndpointTypeSTT},
		{"audio/speech", core.EndpointTypeTTS},
		{"images/generations", core.EndpointTypeImageGeneration},
		{"batches", core.EndpointTypeBatch},
		{"unknown/path", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got := core.EndpointTypeFromPath(tc.path)
		if got != tc.want {
			t.Errorf("EndpointTypeFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestChatOnly_SupportsEndpoint verifies the ChatOnly helper's boundary.
func TestChatOnly_SupportsEndpoint(t *testing.T) {
	var h core.ChatOnly
	if !h.SupportsEndpoint(core.EndpointTypeChat) {
		t.Error("ChatOnly must support chat endpoint")
	}
	if !h.SupportsEndpoint("") {
		t.Error("ChatOnly must support empty endpoint (backward-compat)")
	}
	if h.SupportsEndpoint(core.EndpointTypeEmbeddings) {
		t.Error("ChatOnly must not support embeddings endpoint")
	}
	if h.SupportsEndpoint(core.EndpointTypeBatch) {
		t.Error("ChatOnly must not support batch endpoint")
	}
}

// TestTextOnlyContentScanning_SupportsEndpoint verifies the
// TextOnlyContentScanning helper's endpoint boundary (Class A text hooks).
//
// EndpointTypeEmbeddings is now INCLUDED because embedding
// request inputs are plain text and must be inspectable by PII / keyword /
// safety hooks. The response side is protected separately: BuildPipeline gates
// out any hook implementing TextOnlyContentScanningMarker when stage=response
// and endpointType=embeddings (vectors are not text; see policy.go).
func TestTextOnlyContentScanning_SupportsEndpoint(t *testing.T) {
	var h core.TextOnlyContentScanning
	included := []core.EndpointType{
		core.EndpointTypeChat,
		core.EndpointTypeSTT,
		core.EndpointTypeImageGeneration,
		core.EndpointTypeTTS,
		core.EndpointTypeVideoGeneration,
		core.EndpointTypeEmbeddings, // embedding inputs are text
		"",                          // backward-compat empty
	}
	excluded := []core.EndpointType{
		core.EndpointTypeBatch,
		core.EndpointTypeJob,
	}
	for _, ep := range included {
		if !h.SupportsEndpoint(ep) {
			t.Errorf("TextOnlyContentScanning must support endpoint %q", ep)
		}
	}
	for _, ep := range excluded {
		if h.SupportsEndpoint(ep) {
			t.Errorf("TextOnlyContentScanning must NOT support endpoint %q", ep)
		}
	}
}

// TestAnyEndpointAnyModality verifies the AnyEndpointAnyModality helper.
func TestAnyEndpointAnyModality_AlwaysTrue(t *testing.T) {
	var h core.AnyEndpointAnyModality
	for _, ep := range []core.EndpointType{
		core.EndpointTypeChat, core.EndpointTypeEmbeddings,
		core.EndpointTypeBatch, core.EndpointTypeJob, "",
	} {
		if !h.SupportsEndpoint(ep) {
			t.Errorf("AnyEndpointAnyModality.SupportsEndpoint(%q) must return true", ep)
		}
	}
	for _, m := range []core.Modality{
		core.ModalityText, core.ModalityImage, core.ModalityAudio, core.ModalityVideo, "",
	} {
		if !h.SupportsModality(m) {
			t.Errorf("AnyEndpointAnyModality.SupportsModality(%q) must return true", m)
		}
	}
}
