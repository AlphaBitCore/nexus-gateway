package vkauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

func TestExtractVKToken_NexusHeaderAlwaysWins(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatAnthropic)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-nexus-virtual-key", "nvk_primary")
	req.Header.Set("x-api-key", "nvk_fallback")
	req.Header.Set("Authorization", "Bearer nvk_bearer")

	if got := extractVKToken(ctx, req); got != "nvk_primary" {
		t.Errorf("extractVKToken = %q, want nvk_primary", got)
	}
}

func TestExtractVKToken_BearerBeatsFormatCarrier(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatAnthropic)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer nvk_bearer")
	req.Header.Set("x-api-key", "nvk_xapikey")

	if got := extractVKToken(ctx, req); got != "nvk_bearer" {
		t.Errorf("extractVKToken = %q, want nvk_bearer", got)
	}
}

func TestExtractVKToken_Anthropic_XApiKey(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatAnthropic)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "nvk_xapikey")

	if got := extractVKToken(ctx, req); got != "nvk_xapikey" {
		t.Errorf("extractVKToken = %q, want nvk_xapikey", got)
	}
}

func TestExtractVKToken_Gemini_Header(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatGemini)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", nil)
	req.Header.Set("x-goog-api-key", "nvk_goog")

	if got := extractVKToken(ctx, req); got != "nvk_goog" {
		t.Errorf("extractVKToken = %q, want nvk_goog", got)
	}
}

// The Gemini `?key=<vk>` URL-query carrier is NOT accepted — a
// bearer credential must never be read from the URL (it leaks into logs /
// history / Referer). Only the x-goog-api-key header carries the VK.
func TestExtractVKToken_Gemini_QueryParam_NotAccepted(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatGemini)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent?key=nvk_query", nil)

	if got := extractVKToken(ctx, req); got != "" {
		t.Errorf("extractVKToken = %q, want \"\" (URL ?key= carrier must be rejected)", got)
	}
	// The header carrier still works.
	req.Header.Set("x-goog-api-key", "nvk_header")
	if got := extractVKToken(ctx, req); got != "nvk_header" {
		t.Errorf("extractVKToken = %q, want nvk_header (header carrier)", got)
	}
}

func TestExtractVKToken_Azure_ApiKey(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatAzureOpenAI)
	req := httptest.NewRequest(http.MethodPost, "/openai/deployments/x/chat/completions", nil)
	req.Header.Set("api-key", "nvk_azure")

	if got := extractVKToken(ctx, req); got != "nvk_azure" {
		t.Errorf("extractVKToken = %q, want nvk_azure", got)
	}
}

func TestExtractVKToken_FormatCarrier_IgnoredOnOpenAIRoute(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatOpenAI)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("x-api-key", "nvk_anthropic_style")

	if got := extractVKToken(ctx, req); got != "" {
		t.Errorf("extractVKToken = %q, want empty (x-api-key not honoured on openai route)", got)
	}
}

func TestExtractVKToken_NoIngressFormat_OnlyStandardCarriers(t *testing.T) {
	// No ingress context (e.g. /v1/ai-guard/classify) → only
	// x-nexus-virtual-key + Bearer are honoured.
	req := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/classify", nil)
	req.Header.Set("x-api-key", "nvk_xapikey")
	if got := extractVKToken(context.Background(), req); got != "" {
		t.Errorf("extractVKToken = %q, want empty", got)
	}
	req.Header.Set("Authorization", "Bearer nvk_bearer")
	if got := extractVKToken(context.Background(), req); got != "nvk_bearer" {
		t.Errorf("extractVKToken = %q, want nvk_bearer", got)
	}
}
