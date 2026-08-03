package executor

// The bridge-translated leg's coercion visibility: per-model rules execute
// inside the bridge's codec encode, the adapter's own re-entry differential
// is idempotent (reports nothing), so the executor MUST merge the bridge's
// rewrites into the winning attempt's Coerced or the x-nexus-coerced
// signal silently vanishes on every cross-format attempt.

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestExecutor_BridgedAttempt_CarriesBridgeRewrites(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := &mockResolver{target: provcore.CallTarget{
		ProviderName:    providerSlug,
		Format:          provcore.FormatOpenAI,
		BaseURL:         "https://api.example.com",
		APIKey:          "sk-123",
		ProviderModelID: "o3",
	}}
	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(slog.Default()))
	exec := New(reg, res, nil, bridge)

	req := provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic, // forces the bridge leg
		Body:       []byte(`{"model":"o3","max_tokens":64,"temperature":0.2,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
	}
	tgt := target(providerSlug)
	tgt.ProviderModelID = "o3"

	result := exec.Execute(context.Background(), []routingcore.RoutingTarget{tgt}, req, configtypes.DefaultRetryPolicy())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}
	want := map[string]bool{"max_tokens→max_completion_tokens": true, "temperature→removed": true}
	if len(result.Coerced) != 2 || !want[result.Coerced[0]] || !want[result.Coerced[1]] {
		t.Fatalf("bridge rewrites must reach the winning attempt's Coerced: %v", result.Coerced)
	}
}

// A retry after a failed prepared-body first attempt re-executes the SAME
// prepared bytes through adapter.Execute, whose idempotent re-entry
// reports nothing — the cache-prep rewrites must be merged back onto the
// winning retry or the caller's x-nexus-coerced vanishes exactly when the
// request needed a second attempt.
func TestExecutor_PreparedRetry_CarriesPreparedRewrites(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	result := exec.ExecuteWithPreparedBody(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)}, baseReq(),
		fastBackoffPolicy(2, configtypes.ErrorClass5xx),
		[]byte(`{"model":"gpt-4","messages":[]}`),
		[]string{"temperature→removed"}, "")

	if result.Error != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("expected retry success, got status=%d err=%v", result.StatusCode, result.Error)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	if len(result.Coerced) != 1 || result.Coerced[0] != "temperature→removed" {
		t.Fatalf("the prepared rewrites must survive onto the winning retry: %v", result.Coerced)
	}
}
