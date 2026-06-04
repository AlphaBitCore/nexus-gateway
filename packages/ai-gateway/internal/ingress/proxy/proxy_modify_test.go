package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// openAIIngress is the canonical OpenAI-compat chat/completions ingress
// used by the hook unit tests below. It mirrors what the `/v1/chat/completions`
// route registers in main.go so `runRequestHooks` selects the correct
// traffic adapter.
var openAIIngress = Ingress{
	WireShape:  typology.WireShapeOpenAIChat,
	BodyFormat: provcore.FormatOpenAI,
}

// newPiiRedactHookCache builds a HookConfigCache that serves exactly one
// PII-redact hook, used to drive runRequestHooks through the Modify path
// without spinning up the rest of the proxy.
func newPiiRedactHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "pii-1",
			ImplementationID:  "pii-detector",
			Name:              "pii-detect",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				// Canonical onMatch shape (replaces the legacy `action: "redact"`
				// flat field that ParseOnMatch stopped reading in 8cbc9097).
				// storageAction=redact mirrors the inflight intent so the audit
				// copy also gets the email stripped — matches the prod
				// pii-scanner row post-Fix-7.
				"onMatch": map[string]any{
					"inflightAction": "redact",
					"storageAction":  "redact",
				},
				"patternDefinitions": []any{
					map[string]any{
						"id":          "email",
						"regex":       `[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`,
						"flags":       "i",
						"replacement": "[REDACTED_EMAIL]",
					},
				},
			},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	// Give the first load a moment to populate.
	time.Sleep(50 * time.Millisecond)
	return cache
}

func TestRunRequestHooks_Modify_RewritesBody(t *testing.T) {
	cache := newPiiRedactHookCache(t)
	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"ping alice@example.com"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-test"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if rejected {
		t.Fatalf("unexpected rejection; response=%s", rec.Body.String())
	}
	if rewritten == nil {
		t.Fatal("expected rewritten body, got nil (Modify not wired)")
	}
	got := gjson.GetBytes(rewritten, "messages.0.content").String()
	if got != "ping [REDACTED_EMAIL]" {
		t.Errorf("rewritten content = %q, want %q", got, "ping [REDACTED_EMAIL]")
	}
	if !auditRec.HookRewritten {
		t.Error("audit.HookRewritten should be true")
	}
	if auditRec.HookRewriteCount != 1 {
		t.Errorf("audit.HookRewriteCount = %d, want 1", auditRec.HookRewriteCount)
	}
	if auditRec.HookDecision != string(goHooks.Modify) {
		t.Errorf("audit.RequestHookDecision = %q, want %q", auditRec.HookDecision, string(goHooks.Modify))
	}
}

func TestRunRequestHooks_NoHooks_ReturnsOriginalBody(t *testing.T) {
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) { return nil, nil }
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-test"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if rejected {
		t.Fatalf("unexpected rejection")
	}
	if rewritten != nil {
		t.Errorf("rewritten should be nil when no hooks modify, got %q", string(rewritten))
	}
	if auditRec.HookRewritten {
		t.Error("HookRewritten should be false when no rewrite occurred")
	}
}

// captureHook records the last HookInput it received so tests can assert on
// pipeline-populated fields (SourceIP, ProviderRegion, Model, …) without
// reaching into the pipeline internals.
type captureHook struct {
	goHooks.AnyEndpointAnyModality
	mu     sync.Mutex
	inputs []*goHooks.HookInput
}

func (c *captureHook) Execute(_ context.Context, input *goHooks.HookInput) (*goHooks.HookResult, error) {
	c.mu.Lock()
	snap := *input
	c.inputs = append(c.inputs, &snap)
	c.mu.Unlock()
	return &goHooks.HookResult{Decision: goHooks.Approve}, nil
}

func (c *captureHook) last() *goHooks.HookInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inputs) == 0 {
		return nil
	}
	return c.inputs[len(c.inputs)-1]
}

// newCaptureHookCache builds a HookConfigCache backed by a cloned registry
// that includes a "capture-hook" factory sharing the provided sink. The
// returned sink receives every HookInput the pipeline passes through so
// that tests can assert the runtime populates SourceIP/ProviderRegion.
func newCaptureHookCache(t *testing.T, stage string, sink *captureHook) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("capture-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return sink, nil
	})
	reg.Freeze()

	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "capture-1",
			ImplementationID:  "capture-hook",
			Name:              "capture",
			Priority:          1,
			Enabled:           true,
			Stage:             stage,
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
	time.Sleep(50 * time.Millisecond)
	return cache
}

// TestRunRequestHooks_PopulatesSourceIPAndProviderRegion verifies that the
// AI gateway passes client IP and provider region into the request-stage
// hook input, so data-residency / ip-access hooks receive authoritative
// runtime metadata rather than blanks.
func TestRunRequestHooks_PopulatesSourceIPAndProviderRegion(t *testing.T) {
	sink := &captureHook{}
	cache := newCaptureHookCache(t, "request", sink)
	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.42:51234"
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-ip-region"}

	target := routingcore.RoutingTarget{
		ProviderID:   "prov-1",
		ProviderName: "openai",
		// Hook configs match against the customer-facing code (Model.code),
		// so the runRequestHooks consumer reads ModelCode rather than ModelID
		// (UUID). Test fixture mirrors that.
		ModelID:   "ec3d-uuid",
		ModelCode: "gpt-4",
		Region:    "us-east-1",
	}

	_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-ip-region", body, target, openAIIngress, slog.Default())
	if rejected {
		t.Fatalf("unexpected rejection; response=%s", rec.Body.String())
	}

	got := sink.last()
	if got == nil {
		t.Fatal("capture hook never ran")
	}
	if got.SourceIP != "203.0.113.42" {
		t.Errorf("HookInput.SourceIP = %q, want %q", got.SourceIP, "203.0.113.42")
	}
	if got.ProviderRegion != "us-east-1" {
		t.Errorf("HookInput.ProviderRegion = %q, want %q", got.ProviderRegion, "us-east-1")
	}
	if got.Stage != "request" {
		t.Errorf("HookInput.Stage = %q, want %q", got.Stage, "request")
	}
	if got.IngressType != "AI_GATEWAY" {
		t.Errorf("HookInput.IngressType = %q, want %q", got.IngressType, "AI_GATEWAY")
	}
	if got.Model != "gpt-4" {
		t.Errorf("HookInput.Model = %q, want %q", got.Model, "gpt-4")
	}
}

// rejectingHook rejects every request with a fixed BlockingRule. Used
// to drive the handler → audit.Record propagation path so we can assert
// that `rec.RequestBlockingRule` is populated on rule-pack rejections.
type rejectingHook struct {
	goHooks.AnyEndpointAnyModality
	rule *goHooks.BlockingRule
}

func (r *rejectingHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{
		Decision:     goHooks.RejectHard,
		Reason:       "blocked by rule pack",
		ReasonCode:   "RULEPACK_MATCH",
		BlockingRule: r.rule,
	}, nil
}

// newRejectingHookCache wires a HookConfigCache serving a single rejecting
// request-stage hook that always returns RejectHard with the supplied
// BlockingRule. The hook registry is cloned so this test-only hook does
// not leak into other tests that rely on the canonical registry.
func newRejectingHookCache(t *testing.T, br *goHooks.BlockingRule) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("rejecting-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return &rejectingHook{rule: br}, nil
	})
	reg.Freeze()

	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "reject-1",
			ImplementationID:  "rejecting-hook",
			Name:              "reject",
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
	time.Sleep(50 * time.Millisecond)
	return cache
}

// TestRunRequestHooks_BlockingRulePropagatesToAudit verifies that when a
// request-stage hook (e.g. the rule-pack engine) rejects with
// HookResult.RequestBlockingRule set, the audit record copies the pack/version/
// rule_id tuple into Record.RequestBlockingRule. This is the seam that makes
// `traffic_event.blocking_rule` observable downstream.
func TestRunRequestHooks_BlockingRulePropagatesToAudit(t *testing.T) {
	want := &goHooks.BlockingRule{
		Pack:        "content-safety",
		PackVersion: "1.0.0",
		RuleID:      "violence-kill",
		Category:    "safety",
		Severity:    "hard",
	}
	cache := newRejectingHookCache(t, want)
	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-blocking-rule"}

	_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-blocking-rule", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if !rejected {
		t.Fatalf("expected hook to reject the request; response=%s", rec.Body.String())
	}
	if auditRec.BlockingRule == nil {
		t.Fatal("audit.Record.RequestBlockingRule is nil; expected pack attribution")
	}
	if auditRec.BlockingRule.Pack != want.Pack ||
		auditRec.BlockingRule.PackVersion != want.PackVersion ||
		auditRec.BlockingRule.RuleID != want.RuleID {
		t.Errorf("Record.RequestBlockingRule = %+v, want pack=%q version=%q rule=%q",
			auditRec.BlockingRule, want.Pack, want.PackVersion, want.RuleID)
	}
	if auditRec.HookDecision != string(goHooks.RejectHard) {
		t.Errorf("HookDecision = %q, want %q", auditRec.HookDecision, goHooks.RejectHard)
	}
}

// TestRunRequestHooks_PrefersXForwardedFor verifies that the IP the hook
// pipeline sees is the first X-Forwarded-For hop when the request came
// through a trusted proxy, not the TCP peer.
func TestRunRequestHooks_PrefersXForwardedFor(t *testing.T) {
	sink := &captureHook{}
	cache := newCaptureHookCache(t, "request", sink)
	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.RemoteAddr = "10.0.0.5:44444"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.5")
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-xff"}

	target := routingcore.RoutingTarget{Region: "eu-west-1"}

	_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-xff", body, target, openAIIngress, slog.Default())
	if rejected {
		t.Fatalf("unexpected rejection")
	}

	got := sink.last()
	if got == nil {
		t.Fatal("capture hook never ran")
	}
	if got.SourceIP != "198.51.100.7" {
		t.Errorf("HookInput.SourceIP = %q, want %q", got.SourceIP, "198.51.100.7")
	}
	if got.ProviderRegion != "eu-west-1" {
		t.Errorf("HookInput.ProviderRegion = %q, want %q", got.ProviderRegion, "eu-west-1")
	}
}

// TestRunRequestHooks_RejectHard_WritesHookMarker verifies that when a
// request-stage hook rejects with RejectHard the response includes
// X-Nexus-Hook: rejected:<hookName>:<reasonCode> and X-Nexus-Via
// containing "ai-gateway", even though no upstream was reached.
func TestRunRequestHooks_RejectHard_WritesHookMarker(t *testing.T) {
	br := &goHooks.BlockingRule{
		Pack:     "content-safety",
		RuleID:   "violence",
		Severity: "hard",
	}
	cache := newRejectingHookCache(t, br)
	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-reject-marker"}

	_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-reject-marker", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if !rejected {
		t.Fatal("expected hook to reject the request")
	}

	hookHdr := rec.Header().Get("X-Nexus-Hook")
	if hookHdr == "" {
		t.Error("X-Nexus-Hook header is absent on rejected response")
	}
	if !strings.HasPrefix(hookHdr, "rejected:") {
		t.Errorf("X-Nexus-Hook = %q, want prefix %q", hookHdr, "rejected:")
	}
	// The hook name must appear in the header value.
	if !strings.Contains(hookHdr, "reject") {
		t.Errorf("X-Nexus-Hook = %q, expected hook name 'reject' to appear", hookHdr)
	}

	viaHdr := rec.Header().Get("X-Nexus-Via")
	if !strings.Contains(viaHdr, "ai-gateway") {
		t.Errorf("X-Nexus-Via = %q, expected to contain 'ai-gateway'", viaHdr)
	}

	if rec.Code != http.StatusForbidden {
		t.Errorf("response status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestRunRequestHooks_BlockSoft_WritesHookMarker verifies that the soft-reject
// path (HTTP 246) also emits X-Nexus-Hook and X-Nexus-Via.
func TestRunRequestHooks_BlockSoft_WritesHookMarker(t *testing.T) {
	reg := builtins.Registry.Clone()
	reg.Register("reject-soft-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return &rejectSoftHookImpl{}, nil
	})
	reg.Freeze()

	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "soft-1",
			ImplementationID:  "reject-soft-hook",
			Name:              "soft-reject",
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
	time.Sleep(50 * time.Millisecond)

	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-soft-marker"}

	_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-soft-marker", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if !rejected {
		t.Fatal("expected hook to reject (soft) the request")
	}

	hookHdr := rec.Header().Get("X-Nexus-Hook")
	if hookHdr == "" {
		t.Error("X-Nexus-Hook header is absent on soft-rejected response")
	}
	if !strings.HasPrefix(hookHdr, "rejected:") {
		t.Errorf("X-Nexus-Hook = %q, want prefix %q", hookHdr, "rejected:")
	}

	viaHdr := rec.Header().Get("X-Nexus-Via")
	if !strings.Contains(viaHdr, "ai-gateway") {
		t.Errorf("X-Nexus-Via = %q, expected to contain 'ai-gateway'", viaHdr)
	}

	if rec.Code != 246 {
		t.Errorf("response status = %d, want 246", rec.Code)
	}
}

// rejectSoftHookImpl is a test hook that always returns BlockSoft.
type rejectSoftHookImpl struct {
	goHooks.AnyEndpointAnyModality
}

func (r *rejectSoftHookImpl) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{
		Decision:   goHooks.BlockSoft,
		Reason:     "flagged by compliance",
		ReasonCode: "policy-violation",
	}, nil
}
