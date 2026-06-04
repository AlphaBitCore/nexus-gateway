package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// fakeMetricsRecorder records the arguments of every metric call so
// tests can assert that RecordHookRequest / RecordTrafficExtract were
// invoked with the right ingress-format label.
type fakeMetricsRecorder struct {
	mu           sync.Mutex
	hookRequests []hookRequestCall
	extractCalls []extractCall
}

type hookRequestCall struct {
	ingressFormat string
	stage         string
	decision      string
}

type extractCall struct {
	ingressFormat string
	direction     string
	outcome       string
}

func (f *fakeMetricsRecorder) RecordRequest(_, _, _ string, _ int, _ time.Duration, _ metrics.Usage) {
}

// Compile-time assertion that fakeMetricsRecorder satisfies
// handler.MetricsRecorder — the interface the Handler actually expects.
var _ MetricsRecorder = (*fakeMetricsRecorder)(nil)

func (f *fakeMetricsRecorder) RecordHookRequest(ingressFormat, stage, decision string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hookRequests = append(f.hookRequests, hookRequestCall{ingressFormat, stage, decision})
}

func (f *fakeMetricsRecorder) RecordTrafficExtract(ingressFormat, direction, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extractCalls = append(f.extractCalls, extractCall{ingressFormat, direction, outcome})
}

func (f *fakeMetricsRecorder) RecordEstimate(_, _, _ string, _ time.Duration) {}

func (f *fakeMetricsRecorder) RecordEstimateCompare(_ string, _ int, _ time.Duration) {}

// buildApprovePipeline builds a hook cache containing a single hook
// that always decides "approve". The pipeline's only job in these
// tests is to drive the handler through its extract-adapter path so
// we can observe which traffic adapter the handler selected.
func buildApprovePipeline(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("approve-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return &approveHook{}, nil
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "approve-1",
			ImplementationID:  "approve-hook",
			Name:              "approve",
			Priority:          1,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config:            map[string]any{},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, reg, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	return cache
}

type approveHook struct {
	goHooks.AnyEndpointAnyModality
}

func (approveHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{Decision: goHooks.Approve}, nil
}

// TestRunRequestHooks_UsesIngressFormatTrafficAdapter verifies the
// headline S5 regression: with a registered traffic adapter registry,
// `runRequestHooks` selects the adapter whose ID matches the ingress
// BodyFormat — Anthropic-in uses the `anthropic` adapter, GLM-in uses
// `glm`, not the hard-coded `openai-compat`. The fake metric
// recorder also confirms the `ingress_format` label propagates.
func TestRunRequestHooks_UsesIngressFormatTrafficAdapter(t *testing.T) {
	cache := buildApprovePipeline(t)

	reg := traffic.NewAdapterRegistry("nexus_ai_gateway_test_hook_format")
	adapters.RegisterBuiltins(reg)
	reg.Freeze()

	cases := []struct {
		name       string
		ingress    Ingress
		bodyJSON   string
		wantFormat string
	}{
		{
			name: "anthropic ingress",
			ingress: Ingress{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: provcore.FormatAnthropic,
			},
			bodyJSON: `{
				"model": "claude-3-5-sonnet",
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "hello anthropic"}]}
				]
			}`,
			wantFormat: "anthropic",
		},
		{
			name: "glm ingress",
			ingress: Ingress{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: provcore.FormatGLM,
			},
			bodyJSON: `{
				"model": "glm-4",
				"messages": [
					{"role": "user", "content": "hello glm"}
				]
			}`,
			wantFormat: "glm",
		},
		{
			name: "gemini ingress",
			ingress: Ingress{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: provcore.FormatGemini,
			},
			bodyJSON: `{
				"contents": [
					{"role": "user", "parts": [{"text": "hello gemini"}]}
				]
			}`,
			wantFormat: "gemini",
		},
		{
			name: "openai ingress",
			ingress: Ingress{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: provcore.FormatOpenAI,
			},
			bodyJSON: `{
				"model": "gpt-4",
				"messages": [
					{"role": "user", "content": "hello openai"}
				]
			}`,
			wantFormat: "openai",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics := &fakeMetricsRecorder{}
			h := &Handler{deps: &Deps{
				HookConfigCache: cache,
				TrafficAdapters: reg,
				Metrics:         metrics,
				Logger:          slog.Default(),
			}}

			body := []byte(tc.bodyJSON)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
			rec := httptest.NewRecorder()
			auditRec := &audit.Record{RequestID: "req-" + tc.name}

			_, _, rejected := h.runRequestHooks(req, rec, auditRec, auditRec.RequestID, body, routingcore.RoutingTarget{}, tc.ingress, slog.Default())
			if rejected {
				t.Fatalf("unexpected rejection")
			}

			metrics.mu.Lock()
			defer metrics.mu.Unlock()
			if len(metrics.hookRequests) != 1 {
				t.Fatalf("expected 1 RecordHookRequest call, got %d: %+v", len(metrics.hookRequests), metrics.hookRequests)
			}
			if got := metrics.hookRequests[0].ingressFormat; got != tc.wantFormat {
				t.Errorf("RecordHookRequest ingress_format = %q, want %q", got, tc.wantFormat)
			}
			if got := metrics.hookRequests[0].decision; got != string(goHooks.Approve) {
				t.Errorf("RecordHookRequest decision = %q, want approve", got)
			}
			if len(metrics.extractCalls) == 0 {
				t.Fatalf("expected at least one RecordTrafficExtract call")
			}
			if got := metrics.extractCalls[0].ingressFormat; got != tc.wantFormat {
				t.Errorf("RecordTrafficExtract ingress_format = %q, want %q", got, tc.wantFormat)
			}
			if got := metrics.extractCalls[0].direction; got != "request" {
				t.Errorf("RecordTrafficExtract direction = %q, want request", got)
			}

			// sanity: body should still be a valid JSON after the pipeline.
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Errorf("body JSON invalid after hook approve: %v", err)
			}
		})
	}
}
