package models

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// TestBuildOpenAIModelsResponse_OwnedByIsProviderSlug pins that
// owned_by must be the human-readable provider slug ("openai",
// "anthropic", …), NOT the internal UUID. Strict OpenAI SDK consumers
// compare owned_by to known strings; the AI Gateway Simulator UI also
// groups by this field.
func TestBuildOpenAIModelsResponse_OwnedByIsProviderSlug(t *testing.T) {
	displayOpenAI := "OpenAI"
	rows := []store.Model{
		{
			ID:                  "abc-123-uuid",
			Code:                "gpt-5.2",
			Name:                "GPT-5.2",
			ProviderID:          "prov-uuid-aaaa-bbbb",
			ProviderName:        "openai",
			ProviderDisplayName: &displayOpenAI,
		},
		{
			ID:           "def-456-uuid",
			Code:         "claude-sonnet-4-6",
			Name:         "Claude Sonnet 4.6",
			ProviderID:   "prov-uuid-cccc-dddd",
			ProviderName: "anthropic",
		},
	}
	resp := buildOpenAIModelsResponse(rows)
	if resp["object"] != "list" {
		t.Errorf("object = %v, want list", resp["object"])
	}
	data, ok := resp["data"].([]openAIModelEntry)
	if !ok || len(data) != 2 {
		t.Fatalf("data shape unexpected: %#v", resp["data"])
	}
	if data[0].OwnedBy != "openai" {
		t.Errorf("data[0].OwnedBy = %q, want 'openai' (provider slug, not UUID)", data[0].OwnedBy)
	}
	if data[1].OwnedBy != "anthropic" {
		t.Errorf("data[1].OwnedBy = %q, want 'anthropic'", data[1].OwnedBy)
	}
	// Required OpenAI fields present.
	if data[0].ID != "gpt-5.2" {
		t.Errorf("data[0].ID = %q", data[0].ID)
	}
	if data[0].Object != "model" {
		t.Errorf("data[0].Object = %q, want model", data[0].Object)
	}
	if data[0].Created == 0 {
		t.Error("data[0].Created should be non-zero (Unix timestamp)")
	}
	// Nexus extension fields preserved.
	if data[0].Name != "GPT-5.2" {
		t.Errorf("data[0].Name extension lost: %q", data[0].Name)
	}
	if data[0].OwnerDisplayName == nil || *data[0].OwnerDisplayName != "OpenAI" {
		t.Errorf("data[0].OwnerDisplayName extension lost: %v", data[0].OwnerDisplayName)
	}
	// Second entry has no DisplayName — pointer should be nil.
	if data[1].OwnerDisplayName != nil {
		t.Errorf("data[1].OwnerDisplayName should be nil when Provider.DisplayName absent")
	}
}

// TestBuildAnthropicModelsResponse_ShapeMatchesUpstream pins the
// Anthropic /v1/models shape: data[].type:"model" + display_name +
// created_at, plus top-level first_id/last_id/has_more. Required by
// Claude Code v2.1.129+ — earlier code worked even with the OpenAI
// shape but newer versions silently drop entries missing type:"model".
func TestBuildAnthropicModelsResponse_ShapeMatchesUpstream(t *testing.T) {
	maxCtx := 200_000
	maxOut := 8192
	rows := []store.Model{
		{
			ID:               "uuid-1",
			Code:             "claude-sonnet-4-6",
			Name:             "Claude Sonnet 4.6",
			ProviderID:       "p1",
			ProviderName:     "anthropic",
			MaxContextTokens: &maxCtx,
			MaxOutputTokens:  &maxOut,
		},
	}
	resp := buildAnthropicModelsResponse(rows)
	data, ok := resp["data"].([]anthropicModelEntry)
	if !ok || len(data) != 1 {
		t.Fatalf("data shape unexpected: %#v", resp["data"])
	}
	if data[0].Type != "model" {
		t.Errorf("data[0].Type = %q, want 'model' (Claude Code 2.1.129+ requirement)", data[0].Type)
	}
	if data[0].DisplayName != "Claude Sonnet 4.6" {
		t.Errorf("display_name lost")
	}
	if data[0].CreatedAt == "" {
		t.Error("created_at must be populated (RFC3339)")
	}
	if data[0].MaxInputTokens == nil || *data[0].MaxInputTokens != 200_000 {
		t.Errorf("max_input_tokens lost: %v", data[0].MaxInputTokens)
	}
	if data[0].MaxTokens == nil || *data[0].MaxTokens != 8192 {
		t.Errorf("max_tokens lost: %v", data[0].MaxTokens)
	}
	if resp["has_more"] != false {
		t.Errorf("has_more must default to false")
	}
	if resp["first_id"] != "claude-sonnet-4-6" {
		t.Errorf("first_id = %v", resp["first_id"])
	}
	if resp["last_id"] != "claude-sonnet-4-6" {
		t.Errorf("last_id = %v", resp["last_id"])
	}
}

// TestBuildOpenAIModelsResponse_EmptyList pins the empty-list shape.
func TestBuildOpenAIModelsResponse_EmptyList(t *testing.T) {
	resp := buildOpenAIModelsResponse(nil)
	if resp["object"] != "list" {
		t.Errorf("object = %v", resp["object"])
	}
	data, _ := resp["data"].([]openAIModelEntry)
	if len(data) != 0 {
		t.Errorf("expected empty data, got %d entries", len(data))
	}
}
