package aiguard

import (
	"context"
	"strings"
	"testing"

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
	return &provcore.Response{StatusCode: f.stubStatus, Body: f.stubBody}, nil
}

func (f *fakeAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return &provcore.ProbeResult{OK: true}, nil
}

func (f *fakeAdapter) PrepareBody(req provcore.Request) ([]byte, []string, error) {
	return req.Body, nil, nil
}

func (f *fakeAdapter) ExecuteWithBody(ctx context.Context, req provcore.Request, body []byte, _ []string) (*provcore.Response, error) {
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
//     → Metadata.CostUsd MUST equal the per-token math
//     (PromptTokens × inputPM + CompletionTokens × outputPM) / 1e6.
//
// Together with TestExternalBackend_NoCostStamping_EvenWithUsageInResponse,
// these two lock the rule "ai-guard charges only when calling our internal
// provider AND we have its pricing".
func TestAdapterBackend_StampsCost_OnlyWithPriceLookup(t *testing.T) {
	// Adapter returns usage on the chat-completion response.
	respBody := []byte(`{
		"choices":[{"message":{"content":"{\"decision\":\"approve\"}"}}],
		"usage":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250}
	}`)

	// Case 1 — no PriceLookup wired (fresh deploy / external_url misroute):
	// cost must remain zero even though upstream returned usage.
	{
		a := &fakeAdapter{stubStatus: 200, stubBody: respBody}
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
		a := &fakeAdapter{stubStatus: 200, stubBody: respBody}
		reg := mustRegistry(t, a)
		res := &fakeResolver{target: provcore.CallTarget{
			ProviderName: "openai", Format: provcore.FormatOpenAI,
			BaseURL: "https://api.openai.com", APIKey: "sk-x", ProviderModelID: "gpt-4o-mini",
		}}
		b := &AdapterBackend{
			Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m",
			PriceLookup: func(_ string) (float64, float64) { return 0.15, 0.60 },
		}
		resp, err := b.Call(context.Background(), "x")
		if err != nil {
			t.Fatalf("Case 2 Call: %v", err)
		}
		// 200 × 0.15/M + 50 × 0.60/M = 0.00003 + 0.00003 = 0.00006
		want := (200*0.15 + 50*0.60) / 1_000_000.0
		if resp.Metadata.CostUsd != want {
			t.Errorf("Case 2: CostUsd = %v, want %v", resp.Metadata.CostUsd, want)
		}
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
