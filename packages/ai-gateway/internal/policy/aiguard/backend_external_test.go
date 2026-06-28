// packages/ai-gateway/internal/policy/aiguard/backend_external_test.go
package aiguard

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExternalBackend_HappyPath(t *testing.T) {
	// Mock the upstream OpenAI-compatible endpoint.
	gotAuth := ""
	gotCustomHeader := ""
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustomHeader = r.Header.Get("X-Tenant")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"decision\":\"approve\",\"confidence\":0.9,\"labels\":[\"clean\"]}"}}]}`))
	}))
	defer srv.Close()

	// The external backend has no APIKey field; auth is supplied
	// ONLY through CustomHeaders. Set Authorization there and assert it
	// arrives — proving the operator's own auth channel works and that there
	// is no separate provider-credential bearer path.
	b := &ExternalBackend{
		URL:           srv.URL,
		Model:         "gpt-4o-mini",
		CustomHeaders: map[string]string{"X-Tenant": "nexus", "Authorization": "Bearer judge-token"},
		HTTPClient:    &http.Client{Timeout: 2 * time.Second},
	}
	resp, err := b.Call(context.Background(), "the user said hi")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Decision != "approve" || resp.Confidence != 0.9 {
		t.Errorf("resp: %+v", resp)
	}
	if gotAuth != "Bearer judge-token" {
		t.Errorf("auth (must come from CustomHeaders only): %q", gotAuth)
	}
	if gotCustomHeader != "nexus" {
		t.Errorf("custom header: %q", gotCustomHeader)
	}
	if gotBody["model"] != "gpt-4o-mini" {
		t.Errorf("model: %v", gotBody["model"])
	}
	if _, ok := gotBody["response_format"]; !ok {
		t.Errorf("response_format missing — want json_object hint")
	}

	// Lock in the no-cost-stamping contract. ExternalBackend must NEVER
	// populate Response.Metadata.PromptTokens/CompletionTokens/CostUsd
	// because the gateway has no idea what the operator's external
	// classifier service charges. Future refactors adding usage parsing
	// here would silently start billing customers for ai-guard calls we
	// don't actually pay for.
	if resp.Metadata.PromptTokens != 0 {
		t.Errorf("Metadata.PromptTokens leaked from external upstream: %d (want 0)", resp.Metadata.PromptTokens)
	}
	if resp.Metadata.CompletionTokens != 0 {
		t.Errorf("Metadata.CompletionTokens leaked: %d (want 0)", resp.Metadata.CompletionTokens)
	}
	if resp.Metadata.CostUsd != 0 {
		t.Errorf("Metadata.CostUsd leaked: %v (want 0) — external backend must not bill", resp.Metadata.CostUsd)
	}
}

// TestExternalBackend_NoCostStamping_EvenWithUsageInResponse pins the
// behavior even when the external service returns OpenAI-style usage in
// its response. The gateway must IGNORE it: we don't know the model
// pricing on the external side, so attributing cost would be guessing.
// Customer's ai_guard_cost_usd column stays NULL for external_url mode.
func TestExternalBackend_NoCostStamping_EvenWithUsageInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Response with usage block — tempting bug surface.
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"{\"decision\":\"approve\"}"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}
		}`))
	}))
	defer srv.Close()

	b := &ExternalBackend{
		URL:        srv.URL,
		Model:      "any-model",
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	resp, err := b.Call(context.Background(), "x")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Metadata.PromptTokens != 0 || resp.Metadata.CompletionTokens != 0 || resp.Metadata.CostUsd != 0 {
		t.Fatalf("ExternalBackend stamped usage/cost despite external_url contract: %+v", resp.Metadata)
	}
}

func TestExternalBackend_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()
	b := &ExternalBackend{URL: srv.URL, Model: "m", HTTPClient: &http.Client{Timeout: time.Second}}
	_, err := b.Call(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "status=500") {
		t.Fatalf("expected status=500 error, got %v", err)
	}
}

func TestExternalBackend_MalformedContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not json"}}]}`))
	}))
	defer srv.Close()
	b := &ExternalBackend{URL: srv.URL, Model: "m", HTTPClient: &http.Client{Timeout: time.Second}}
	_, err := b.Call(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "no JSON object") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

// TestExternalBackend_OversizeBodyCapped verifies: a success-path
// response body larger than the 1 MiB LimitReader ceiling is truncated rather
// than buffered whole. The truncated stream is no longer valid JSON, so Call
// returns a parse error instead of consuming unbounded memory.
func TestExternalBackend_OversizeBodyCapped(t *testing.T) {
	// 2 MiB of padding inside a JSON string value that is never closed within
	// the first 1 MiB, guaranteeing the truncated read is invalid JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"`))
		pad := strings.Repeat("A", 2<<20)
		_, _ = w.Write([]byte(pad))
		_, _ = w.Write([]byte(`"}}]}`))
	}))
	defer srv.Close()

	b := &ExternalBackend{URL: srv.URL, Model: "m", HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := b.Call(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected parse error from truncated oversize body, got nil")
	}
	if !strings.Contains(err.Error(), "parse chat response") {
		t.Fatalf("expected parse error (truncated JSON), got %v", err)
	}
}

func TestExternalBackend_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()
	b := &ExternalBackend{URL: srv.URL, Model: "m", HTTPClient: &http.Client{Timeout: 10 * time.Millisecond}}
	_, err := b.Call(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected timeout")
	}
}
