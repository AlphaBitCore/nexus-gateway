package debug

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// stubResolver is an in-memory routeResolver for handler tests. The resolve
// func is invoked verbatim, so each case can assert on the RoutingContext
// that the handler built and return the RoutingPlan it wants. The handler
// calls Explain (not Resolve) because simulate enriches plans with the
// deterministic branch enumeration; the stub keeps the single `resolve`
// hook so the test shape stays familiar and the stub doubles as Explain.
type stubResolver struct {
	resolve func(ctx context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error)
}

func (s *stubResolver) Explain(ctx context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
	return s.resolve(ctx, rctx)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func decodeResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not JSON: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func TestRoutingSimulate_EmptyModelId(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(context.Context, *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			t.Fatal("resolver must not be called when modelId is empty")
			return nil, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := decodeResp(t, rec)
	if body["error"] != "modelId is required" {
		t.Errorf("unexpected error: %v", body["error"])
	}
}

func TestRoutingSimulate_MalformedJSON(t *testing.T) {
	h := RoutingSimulateHandler(&stubResolver{
		resolve: func(context.Context, *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			t.Fatal("resolver must not be called on malformed body")
			return nil, nil
		},
	}, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRoutingSimulate_NoRuleMatched(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				PipelineTrace: []routingcore.PipelineTraceEntry{
					{Stage: 1, Decision: "no rule matched", DurationMs: 1},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"unknown","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeResp(t, rec)
	targets, _ := body["targets"].([]any)
	if len(targets) != 0 {
		t.Errorf("targets should be empty, got %d", len(targets))
	}
	warnings, _ := body["warnings"].([]any)
	if len(warnings) < 2 {
		t.Fatalf("expected at least 2 warnings, got %v", warnings)
	}
	if !strings.Contains(warnings[0].(string), "no stage-1 rule matched") {
		t.Errorf("expected first warning about no stage-1 rule, got %v", warnings[0])
	}
	if !strings.Contains(warnings[1].(string), "without virtual-key context") {
		t.Errorf("expected virtual-key-context warning, got %v", warnings[1])
	}
	if _, ok := body["ruleId"]; ok {
		t.Errorf("ruleId must be omitted when no rule matched")
	}
}

func TestRoutingSimulate_SingleStrategyMatch(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				RuleID:          "rr-1",
				RuleName:        "OpenAI chat baseline",
				PipelineTrace: []routingcore.PipelineTraceEntry{
					{Stage: 1, Decision: "rule matched", DurationMs: 2},
				},
				Trace: []routingcore.TraceEntry{
					{RuleID: "rr-1", RuleName: "OpenAI chat baseline", StrategyType: "direct", Decision: "selected target", DurationMs: 1},
				},
				Targets: []routingcore.RoutingTarget{
					{
						ProviderID: "prov-1", ProviderName: "openai",
						ModelID: "mdl-1", ModelName: "gpt-4o-mini",
						ProviderModelID: "gpt-4o-mini", Source: "primary",
					},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"gpt-4o-mini","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeResp(t, rec)
	if body["ruleId"] != "rr-1" || body["ruleName"] != "OpenAI chat baseline" {
		t.Errorf("rule identity missing: %v / %v", body["ruleId"], body["ruleName"])
	}
	targets, _ := body["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	first, _ := targets[0].(map[string]any)
	if first["providerId"] != "prov-1" || first["modelId"] != "mdl-1" || first["source"] != "primary" {
		t.Errorf("unexpected target shape: %#v", first)
	}
	warnings, _ := body["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "without virtual-key context") {
		t.Errorf("expected only virtual-key-context warning, got %v", warnings)
	}
}

func TestRoutingSimulate_AutoWithMessages(t *testing.T) {
	var gotRequest *normalize.NormalizedPayload
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			gotRequest = rctx.Request
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				RuleID:          "rr-smart",
				RuleName:        "Smart default",
				Substituted:     true,
				PipelineTrace: []routingcore.PipelineTraceEntry{
					{Stage: 1, Decision: "smart rule matched", DurationMs: 4},
				},
				Targets: []routingcore.RoutingTarget{
					{
						ProviderID: "prov-1", ProviderName: "openai",
						ModelID: "mdl-mini", ModelName: "gpt-4o-mini",
						ProviderModelID: "gpt-4o-mini", Source: "primary",
					},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"auto","endpointType":"chat","messages":[{"role":"user","content":"Hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeResp(t, rec)
	if body["substituted"] != true {
		t.Errorf("substituted should be true, got %v", body["substituted"])
	}
	// Routing-internal data flows through rctx.Request, not the
	// HTTP-header map. Assert the canonical payload carries the supplied
	// user message.
	if gotRequest == nil {
		t.Fatalf("handler should populate rctx.Request from simulateRequest.Messages; got nil")
	}
	if len(gotRequest.Messages) != 1 || gotRequest.Messages[0].Role != normalize.RoleUser {
		t.Errorf("rctx.Request.Messages[0].Role = %v, want one role=user message; got %#v", "?", gotRequest.Messages)
	}
	if len(gotRequest.Messages[0].Content) == 0 || gotRequest.Messages[0].Content[0].Text != "Hi" {
		t.Errorf("rctx.Request.Messages[0].Content[0].Text = %q, want %q", "?", "Hi")
	}
	warnings, _ := body["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "without virtual-key context") {
		t.Errorf("expected only virtual-key-context warning, got %v", warnings)
	}
}

func TestRoutingSimulate_AutoNoMessages(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			// Empty messages → rctx.Request should be nil; routing
			// strategies fall back gracefully without a canonical payload.
			if rctx.Request != nil {
				t.Errorf("handler must not populate rctx.Request when messages is empty; got %#v", rctx.Request)
			}
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				RuleID:          "rr-smart",
				RuleName:        "Smart default",
				Substituted:     true,
				Targets: []routingcore.RoutingTarget{
					{ProviderID: "p", ProviderName: "p", ModelID: "m", ModelName: "m", ProviderModelID: "m", Source: "primary"},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"auto","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeResp(t, rec)
	warnings, _ := body["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if strings.Contains(w.(string), "smart routing requested but messages is empty") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected smart-no-messages warning, got %v", warnings)
	}
}

func TestRoutingSimulate_NarrowingSummary(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				RuleID:          "rr-chat",
				RuleName:        "Default chat",
				PipelineTrace: []routingcore.PipelineTraceEntry{
					{Stage: 0, Decision: "narrowing applied", DurationMs: 1},
					{Stage: 1, Decision: "chat rule matched", DurationMs: 1},
				},
				NarrowingSummary: &routingcore.NarrowingSummary{
					AllowProviderIDs: []string{"prov-eu"},
					DenyProviderIDs:  []string{"prov-us"},
				},
				Targets: []routingcore.RoutingTarget{
					{
						ProviderID: "prov-eu", ProviderName: "eu",
						ModelID: "mdl-1", ModelName: "gpt-4o",
						ProviderModelID: "gpt-4o", Source: "primary",
					},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"gpt-4o","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeResp(t, rec)
	narrow, ok := body["narrowingSummary"].(map[string]any)
	if !ok {
		t.Fatalf("narrowingSummary missing or wrong type: %v", body["narrowingSummary"])
	}
	allow, _ := narrow["allowProviderIds"].([]any)
	if len(allow) != 1 || allow[0] != "prov-eu" {
		t.Errorf("allowProviderIds wrong: %v", allow)
	}
}

func TestRoutingSimulate_EndpointTypeNormalization(t *testing.T) {
	// rctx.EndpointType carries the canonical typology.EndpointKind;
	// the request-echo in the response body carries the same canonical
	// kind string so admins compare like-for-like with what they sent.
	cases := []struct {
		name     string
		bodyEnd  string
		wantRctx typology.EndpointKind
		wantEcho string
	}{
		{"short form 'chat'", `"chat"`, typology.EndpointKindChat, "chat"},
		{"empty string", `""`, typology.EndpointKindChat, "chat"},
		{"explicit embeddings", `"embeddings"`, typology.EndpointKindEmbeddings, "embeddings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotEnd typology.EndpointKind
			resolver := &stubResolver{
				resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
					gotEnd = rctx.EndpointType
					return &routingcore.RoutingPlan{
						OriginalModelID: rctx.RequestedModel.ID,
						RuleID:          "r1",
						Targets: []routingcore.RoutingTarget{
							{ProviderID: "p", ProviderName: "p", ModelID: "m", ModelName: "m", ProviderModelID: "m", Source: "primary"},
						},
					}, nil
				},
			}
			h := RoutingSimulateHandler(resolver, nil, discardLogger())

			payload := `{"modelId":"gpt-4o-mini","endpointType":` + tc.bodyEnd + `}`
			req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate", strings.NewReader(payload))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if gotEnd != tc.wantRctx {
				t.Errorf("rctx.EndpointType = %q, want %q", gotEnd, tc.wantRctx)
			}
			body := decodeResp(t, rec)
			echoed, _ := body["request"].(map[string]any)
			if echoed["endpointType"] != tc.wantEcho {
				t.Errorf("echoed endpointType = %v, want %q", echoed["endpointType"], tc.wantEcho)
			}
		})
	}
}

func TestRoutingSimulate_ResolverError(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(context.Context, *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			return nil, errors.New("db unreachable")
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"x","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := decodeResp(t, rec)
	if !strings.Contains(body["error"].(string), "db unreachable") {
		t.Errorf("error should surface engine message, got %v", body["error"])
	}
}

// TestRoutingSimulate_BranchesSurfaced verifies that plan.Branches flows
// through to the response as a `branches` array — this is the fix for the
// simulate UX gap where a loadbalance/ab_split rule showed only the
// stochastic pick. Each branch carries providerId, modelId, probability,
// matched, and path so the UI can render the full distribution.
func TestRoutingSimulate_BranchesSurfaced(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				RuleID:          "r-lb",
				RuleName:        "70/30",
				PipelineTrace: []routingcore.PipelineTraceEntry{
					{Stage: 1, Decision: "loadbalance selected 1 of 2 branches", DurationMs: 1},
				},
				Targets: []routingcore.RoutingTarget{
					{ProviderID: "openai", ProviderName: "openai", ModelID: "gpt-4", ModelName: "gpt-4", ProviderModelID: "gpt-4", Source: "primary"},
				},
				Branches: []routingcore.BranchedTarget{
					{
						Target:      routingcore.RoutingTarget{ProviderID: "openai", ModelID: "gpt-4", ProviderModelID: "gpt-4"},
						Probability: 0.7,
						Path:        "loadbalance[0,w=70/100] > single(openai/gpt-4)",
						Matched:     true,
					},
					{
						Target:      routingcore.RoutingTarget{ProviderID: "google", ModelID: "gemini", ProviderModelID: "gemini"},
						Probability: 0.3,
						Path:        "loadbalance[1,w=30/100] > single(google/gemini)",
						Matched:     true,
						Note:        "lookup failed: provider or model disabled",
					},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"gpt-4","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeResp(t, rec)
	branches, ok := body["branches"].([]any)
	if !ok {
		t.Fatalf("branches missing or wrong type: %v", body["branches"])
	}
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	first, _ := branches[0].(map[string]any)
	if first["providerId"] != "openai" || first["modelId"] != "gpt-4" {
		t.Errorf("first branch wrong: %+v", first)
	}
	if p, _ := first["probability"].(float64); p != 0.7 {
		t.Errorf("first branch probability = %v, want 0.7", first["probability"])
	}
	if !strings.Contains(first["path"].(string), "loadbalance") {
		t.Errorf("first branch path should describe the tree walk: %v", first["path"])
	}
	second, _ := branches[1].(map[string]any)
	if second["note"] == nil || !strings.Contains(second["note"].(string), "lookup failed") {
		t.Errorf("second branch should carry a disabled-provider note, got %+v", second)
	}
}

// TestRoutingSimulate_BranchesAlwaysArray pins the empty-state contract:
// even when the matched rule has no branches (e.g. a single-strategy rule),
// the response must still serialise `branches` as a [] array, not omit it.
// Frontends that iterate directly over resp.branches shouldn't crash.
func TestRoutingSimulate_BranchesAlwaysArray(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				RuleID:          "r-single",
				Targets: []routingcore.RoutingTarget{
					{ProviderID: "p", ProviderName: "p", ModelID: "m", ModelName: "m", ProviderModelID: "m", Source: "primary"},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate",
		strings.NewReader(`{"modelId":"m","endpointType":"chat"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := decodeResp(t, rec)
	branches, ok := body["branches"].([]any)
	if !ok {
		t.Fatalf("branches must be a JSON array even when empty, got %#v", body["branches"])
	}
	if len(branches) != 0 {
		t.Errorf("expected empty branches, got %d", len(branches))
	}
}

// Sanity test: the handler returns a content-type of application/json and a
// response body that round-trips into SimulateResponse JSON without unknown
// fields being dropped for non-empty slices.
func TestRoutingSimulate_ContentTypeAndShape(t *testing.T) {
	resolver := &stubResolver{
		resolve: func(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error) {
			return &routingcore.RoutingPlan{
				OriginalModelID: rctx.RequestedModel.ID,
				RuleID:          "r1",
				Targets: []routingcore.RoutingTarget{
					{ProviderID: "p", ProviderName: "p", ModelID: "m", ModelName: "m", ProviderModelID: "m", Source: "primary"},
				},
			}, nil
		},
	}
	h := RoutingSimulateHandler(resolver, nil, discardLogger())

	payload, _ := json.Marshal(map[string]any{"modelId": "x", "endpointType": "chat"})
	req := httptest.NewRequest(http.MethodPost, "/internal/routing-simulate", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body := decodeResp(t, rec)
	// `stages` and `trace` must always be arrays (even when empty) per OpenAPI.
	if _, ok := body["stages"].([]any); !ok {
		t.Errorf("stages must be a JSON array")
	}
	if _, ok := body["trace"].([]any); !ok {
		t.Errorf("trace must be a JSON array")
	}
}

// TestNormalizeEndpointType pins the canonical normalization: empty
// defaults to "chat"; valid canonical kinds pass through unchanged;
// unknown values pass through so the UI can still render a plan for
// endpoint kinds the proxy adds later.
func TestNormalizeEndpointType(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "chat"},
		{"   ", "chat"},
		{"chat", "chat"},
		{"embeddings", "embeddings"},
		{"models", "models"},
		{"future-endpoint", "future-endpoint"}, // pass-through for forward compat
	}
	for _, c := range cases {
		if got := normalizeEndpointType(c.in); got != c.want {
			t.Errorf("normalizeEndpointType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
