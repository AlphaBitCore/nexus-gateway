package aiguard

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

type fakeAdapter struct {
	called     bool
	gotReq     provcore.Request
	stubBody   []byte
	stubStatus int
	stubErr    error
	// stubUsage is the decoded token envelope the real adapter attaches after
	// running the provider's usage block through its alias chain. AdapterBackend
	// reads token counts from here, not from stubBody — scripting them in the
	// body alone would exercise a decoder the backend no longer owns.
	stubUsage provcore.Usage
}

func (f *fakeAdapter) Format() provcore.Format { return provcore.FormatOpenAI }
func (f *fakeAdapter) SupportsShape(shape typology.WireShape) bool {
	return shape == typology.WireShapeOpenAIChat
}

func (f *fakeAdapter) Execute(_ context.Context, req provcore.Request) (*provcore.Response, error) {
	f.called = true
	f.gotReq = req
	if f.stubErr != nil {
		return nil, f.stubErr
	}
	return &provcore.Response{StatusCode: f.stubStatus, Body: f.stubBody, Usage: f.stubUsage}, nil
}

func (f *fakeAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return &provcore.ProbeResult{OK: true}, nil
}

func (f *fakeAdapter) PrepareBody(req provcore.Request) ([]byte, []string, string, error) {
	return req.Body, nil, "", nil
}

func (f *fakeAdapter) ExecuteWithBody(ctx context.Context, req provcore.Request, body []byte, _ []string, _ string) (*provcore.Response, error) {
	req.Body = body
	return f.Execute(ctx, req)
}

type fakeResolver struct {
	target provcore.CallTarget
	err    error
	calls  int
}

func (f *fakeResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	f.calls++
	if f.err != nil {
		return provcore.CallTarget{}, f.err
	}
	t := f.target
	if t.ProviderID == "" {
		t.ProviderID = providerID
	}
	if t.ProviderModelID == "" {
		t.ProviderModelID = modelID
	}
	return t, nil
}

func mustRegistry(t *testing.T, a provcore.Adapter) *provcore.Registry {
	t.Helper()
	r := provcore.NewRegistry()
	if err := r.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.Freeze()
	return r
}

// newTestBackend builds an AdapterBackend wired to a fakeAdapter that returns
// respBody, resolved via a fakeResolver pinned to target. Used by tests that
// only care about the parsed Response, not the adapter/resolver plumbing.
func newTestBackend(t *testing.T, target provcore.CallTarget, respBody string) *AdapterBackend {
	t.Helper()
	if target.Format == "" {
		target.Format = provcore.FormatOpenAI
	}
	a := &fakeAdapter{stubStatus: 200, stubBody: []byte(respBody)}
	reg := mustRegistry(t, a)
	res := &fakeResolver{target: target}
	return &AdapterBackend{
		Resolver:   res,
		Registry:   reg,
		ProviderID: "p",
		ModelID:    "m",
	}
}

func TestAdapterBackend_CallsAdapterDirectly(t *testing.T) {
	a := &fakeAdapter{
		stubStatus: 200,
		stubBody:   []byte(`{"choices":[{"message":{"content":"{\"decision\":\"approve\",\"labels\":[\"ok\"]}"}}]}`),
	}
	reg := mustRegistry(t, a)
	res := &fakeResolver{target: provcore.CallTarget{
		ProviderName:    "openai",
		Format:          provcore.FormatOpenAI,
		BaseURL:         "https://api.openai.com",
		APIKey:          "sk-x",
		ProviderModelID: "gpt-4o-mini",
	}}
	b := &AdapterBackend{
		Resolver:   res,
		Registry:   reg,
		ProviderID: "prov-fake",
		ModelID:    "model-1",
	}
	resp, err := b.Call(context.Background(), "prompt text")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.calls != 1 {
		t.Fatalf("expected 1 resolver call, got %d", res.calls)
	}
	if !a.called {
		t.Fatal("adapter never called")
	}
	if resp.Decision != "approve" {
		t.Fatalf("decision: %q", resp.Decision)
	}
	if a.gotReq.WireShape != typology.WireShapeOpenAIChat {
		t.Errorf("endpoint: %s", a.gotReq.WireShape)
	}
	if a.gotReq.BodyFormat != provcore.FormatOpenAI {
		t.Errorf("body format: %s", a.gotReq.BodyFormat)
	}
	if !strings.Contains(string(a.gotReq.Body), "gpt-4o-mini") {
		t.Errorf("body missing model: %s", a.gotReq.Body)
	}
}

// TestAdapterBackend_StampsCost_OnlyWithPriceLookup pins the cost-stamping
// contract for the configured-provider backend:
//   - Without PriceLookup → Metadata.CostUsd MUST be 0 (we don't know the
//     model's pricing, so we don't bill the customer for an internal
//     classifier call). This is the safe-default case for fresh deploys
//     before the Models snapshot has loaded.
//   - With PriceLookup returning real prices AND upstream returning usage
//     → Metadata.CostUsd MUST equal the per-bucket math, each token bucket
//     at its own rate.
//
// Together with TestExternalBackend_NoCostStamping_EvenWithUsageInResponse,
// these two lock the rule "ai-guard charges only when calling our internal
// provider AND we have its pricing".
func TestAdapterBackend_StampsCost_OnlyWithPriceLookup(t *testing.T) {
	respBody := []byte(`{"choices":[{"message":{"content":"{\"decision\":\"approve\"}"}}]}`)
	pt, ct := 200, 50
	usage := provcore.Usage{PromptTokens: &pt, CompletionTokens: &ct}

	// Case 1 — no PriceLookup wired (fresh deploy / external_url misroute):
	// cost must remain zero even though upstream returned usage.
	{
		a := &fakeAdapter{stubStatus: 200, stubBody: respBody, stubUsage: usage}
		reg := mustRegistry(t, a)
		res := &fakeResolver{target: provcore.CallTarget{
			ProviderName: "openai", Format: provcore.FormatOpenAI,
			BaseURL: "https://api.openai.com", APIKey: "sk-x", ProviderModelID: "gpt-4o-mini",
		}}
		b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
		resp, err := b.Call(context.Background(), "x")
		if err != nil {
			t.Fatalf("Case 1 Call: %v", err)
		}
		if resp.Metadata.CostUsd != 0 {
			t.Errorf("Case 1 (no PriceLookup): CostUsd = %v, want 0", resp.Metadata.CostUsd)
		}
		if resp.Metadata.PromptTokens != 200 || resp.Metadata.CompletionTokens != 50 {
			t.Errorf("Case 1: tokens not parsed — got pt=%d ct=%d",
				resp.Metadata.PromptTokens, resp.Metadata.CompletionTokens)
		}
	}

	// Case 2 — PriceLookup wired with gpt-4o-mini prices: cost stamped.
	{
		a := &fakeAdapter{stubStatus: 200, stubBody: respBody, stubUsage: usage}
		reg := mustRegistry(t, a)
		res := &fakeResolver{target: provcore.CallTarget{
			ProviderName: "openai", Format: provcore.FormatOpenAI,
			BaseURL: "https://api.openai.com", APIKey: "sk-x", ProviderModelID: "gpt-4o-mini",
		}}
		b := &AdapterBackend{
			Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m",
			PriceLookup: func(_ string) (costing.Rates, bool) {
				return costing.Rates{
					InputUSDPerM: 0.15, OutputUSDPerM: 0.60,
					CacheReadUSDPerM: 0.075, CacheWriteUSDPerM: 0.15,
				}, true
			},
		}
		resp, err := b.Call(context.Background(), "x")
		if err != nil {
			t.Fatalf("Case 2 Call: %v", err)
		}
		// No cached tokens reported, so all 200 input bill at the input rate:
		// 200 × 0.15/M + 50 × 0.60/M = 0.00003 + 0.00003 = 0.00006
		want := (200*0.15 + 50*0.60) / 1_000_000.0
		if resp.Metadata.CostUsd != want {
			t.Errorf("Case 2: CostUsd = %v, want %v", resp.Metadata.CostUsd, want)
		}
	}
}

// TestAdapterBackend_CachedJudgePromptBilledAtCacheRate is the regression test
// for the classifier's share of the internal-ops over-estimate. The judge
// template is fixed, so a warm provider cache serves most of the prompt and the
// provider reports prompt_tokens INCLUDING that cached share. The backend used
// to bill every one of those tokens at the full input rate.
func TestAdapterBackend_CachedJudgePromptBilledAtCacheRate(t *testing.T) {
	pt, ct, cr := 4000, 20, 3600
	a := &fakeAdapter{
		stubStatus: 200,
		stubBody:   []byte(`{"choices":[{"message":{"content":"{\"decision\":\"approve\"}"}}]}`),
		stubUsage: provcore.Usage{
			PromptTokens: &pt, CompletionTokens: &ct, CacheReadTokens: &cr,
		},
	}
	reg := mustRegistry(t, a)
	res := &fakeResolver{target: provcore.CallTarget{
		ProviderName: "openai", Format: provcore.FormatOpenAI, ProviderModelID: "gpt-4o-mini",
	}}
	b := &AdapterBackend{
		Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m",
		PriceLookup: func(_ string) (costing.Rates, bool) {
			return costing.Rates{
				InputUSDPerM: 0.15, OutputUSDPerM: 0.60,
				CacheReadUSDPerM: 0.075, CacheWriteUSDPerM: 0.15,
			}, true
		},
	}
	resp, err := b.Call(context.Background(), "x")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Metadata.CacheReadTokens != 3600 {
		t.Errorf("CacheReadTokens = %d, want 3600 — the adapter's decoded cache bucket must reach the sink",
			resp.Metadata.CacheReadTokens)
	}
	// 400 uncached × 0.15 + 3600 cached × 0.075 + 20 out × 0.60, per 1M.
	want := (400*0.15 + 3600*0.075 + 20*0.60) / 1_000_000.0
	if math.Abs(resp.Metadata.CostUsd-want) > 1e-15 {
		t.Errorf("CostUsd = %v, want %v", resp.Metadata.CostUsd, want)
	}
	overEstimate := (4000*0.15 + 20*0.60) / 1_000_000.0
	if resp.Metadata.CostUsd >= overEstimate {
		t.Errorf("CostUsd = %v is not below the full-input-rate figure %v; the cached share is being billed at the input rate again",
			resp.Metadata.CostUsd, overEstimate)
	}
}

func TestAdapterBackend_AdapterError(t *testing.T) {
	a := &fakeAdapter{stubErr: errFakeTest("fake adapter failure")}
	reg := mustRegistry(t, a)
	res := &fakeResolver{target: provcore.CallTarget{ProviderName: "openai", Format: provcore.FormatOpenAI}}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error propagation")
	}
}

func TestAdapterBackend_Non2xx(t *testing.T) {
	a := &fakeAdapter{stubStatus: 429, stubBody: []byte(`{"error":"rate limited"}`)}
	reg := mustRegistry(t, a)
	res := &fakeResolver{target: provcore.CallTarget{ProviderName: "openai", Format: provcore.FormatOpenAI}}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "status=429") {
		t.Fatalf("expected status=429 err, got %v", err)
	}
}

func TestAdapterBackend_ResolverError(t *testing.T) {
	a := &fakeAdapter{stubStatus: 200}
	reg := mustRegistry(t, a)
	res := &fakeResolver{err: errFakeTest("vault offline")}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil {
		t.Fatal("expected resolver error")
	}
	if a.called {
		t.Fatal("adapter must not be called when resolver fails")
	}
}

type errFakeTest string

func (e errFakeTest) Error() string { return string(e) }

// TestAdapterBackend_Call_StampsServingProviderOnMetadata pins the fix for
// the ai-guard attribution gap: AdapterBackend.Call must stamp the resolved
// call target's ProviderID/-Name onto Response.Metadata, sourced from
// target (the Resolver's output), never from AdapterBackend.ProviderID
// (the Nexus-side provider selector, which can legitimately differ from the
// serving provider's identity — e.g. failover) or from the model id.
func TestAdapterBackend_Call_StampsServingProviderOnMetadata(t *testing.T) {
	b := newTestBackend(t, provcore.CallTarget{ProviderID: "prov-openai", ProviderName: "openai"},
		`{"choices":[{"message":{"content":"{\"decision\":\"approve\"}"}}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":10}}`)

	got, err := b.Call(context.Background(), "check this")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Metadata.ProviderID != "prov-openai" {
		t.Errorf("Metadata.ProviderID = %q, want prov-openai", got.Metadata.ProviderID)
	}
	if got.Metadata.ProviderName != "openai" {
		t.Errorf("Metadata.ProviderName = %q, want openai", got.Metadata.ProviderName)
	}
}
