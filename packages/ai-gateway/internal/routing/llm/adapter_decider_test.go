package llm

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// fakeAdapter implements just enough of provcore.Adapter to script
// responses for AdapterDecider's Execute call. The other Adapter methods
// (Probe, PrepareBody, ExecuteWithBody) are no-ops since AdapterDecider
// only invokes Execute.
type fakeAdapter struct {
	format    provcore.Format
	stubResp  *provcore.Response
	stubErr   error
	executeFn func(ctx context.Context, req provcore.Request) (*provcore.Response, error)
	gotReq    provcore.Request
	calls     int
}

func (a *fakeAdapter) Format() provcore.Format { return a.format }
func (a *fakeAdapter) SupportsShape(shape typology.WireShape) bool {
	return shape == typology.WireShapeOpenAIChat
}
func (a *fakeAdapter) Execute(ctx context.Context, req provcore.Request) (*provcore.Response, error) {
	a.calls++
	a.gotReq = req
	if a.executeFn != nil {
		return a.executeFn(ctx, req)
	}
	if a.stubErr != nil {
		return nil, a.stubErr
	}
	return a.stubResp, nil
}
func (a *fakeAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return &provcore.ProbeResult{OK: true}, nil
}
func (a *fakeAdapter) PrepareBody(req provcore.Request) ([]byte, []string, string, error) {
	return req.Body, nil, "", nil
}
func (a *fakeAdapter) ExecuteWithBody(ctx context.Context, req provcore.Request, body []byte, _ []string, _ string) (*provcore.Response, error) {
	req.Body = body
	return a.Execute(ctx, req)
}

// fakeResolver implements provtarget.Resolver with a scripted CallTarget.
type fakeResolver struct {
	target provcore.CallTarget
	err    error
	calls  int
}

func (r *fakeResolver) Resolve(_ context.Context, _, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	r.calls++
	if r.err != nil {
		return provcore.CallTarget{}, r.err
	}
	return r.target, nil
}

// fakeAdapterLookup wraps a single registered adapter.
type fakeAdapterLookup struct {
	byFormat map[provcore.Format]provcore.Adapter
}

func (l *fakeAdapterLookup) Get(f provcore.Format) (provcore.Adapter, bool) {
	a, ok := l.byFormat[f]
	return a, ok
}

func newAdapterLookup(a provcore.Adapter) *fakeAdapterLookup {
	return &fakeAdapterLookup{byFormat: map[provcore.Format]provcore.Adapter{a.Format(): a}}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestAdapterDecider_HappyPath_ReturnsDecision(t *testing.T) {
	adapter := &fakeAdapter{
		format: provcore.FormatOpenAI,
		stubResp: &provcore.Response{
			StatusCode: 200,
			Body: mustJSON(t, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": `{"modelId":"m-claude","reason":"best"}`}}},
			}),
		},
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderName:    "openai",
		Format:          provcore.FormatOpenAI,
		BaseURL:         "https://api.openai.com",
		ProviderModelID: "gpt-4o-mini",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	decision, err := d.Decide(context.Background(), Request{
		SystemPrompt:     "pick",
		Timeout:          50 * time.Millisecond,
		RouterProviderID: "p-router",
		RouterModelID:    "m-router",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.ModelID != "m-claude" || decision.Reason != "best" {
		t.Errorf("got decision %#v", decision)
	}
	if adapter.gotReq.WireShape != typology.WireShapeOpenAIChat {
		t.Errorf("endpoint = %s, want %s", adapter.gotReq.WireShape, typology.WireShapeOpenAIChat)
	}
	if adapter.gotReq.BodyFormat != provcore.FormatOpenAI {
		t.Errorf("BodyFormat = %s, want OpenAI (canonical)", adapter.gotReq.BodyFormat)
	}
}

func TestAdapterDecider_ResolverFails_ErrorTextMatchesTrace(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("vault offline")}
	adapter := &fakeAdapter{format: provcore.FormatOpenAI}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	_, err := d.Decide(context.Background(), Request{Timeout: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected error from resolver failure")
	}
	want := "router target resolve failed: vault offline"
	if err.Error() != want {
		t.Errorf("err.Error() = %q, want %q (error text must match routing_trace vocabulary)", err.Error(), want)
	}
	if adapter.calls != 0 {
		t.Errorf("adapter must not be called when resolver fails; got %d calls", adapter.calls)
	}
}

func TestAdapterDecider_InvalidAdapterType_ErrorTextMatchesTrace(t *testing.T) {
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderName: "wonky",
		Format:       provcore.Format(""), // invalid
	}}
	adapter := &fakeAdapter{format: provcore.FormatOpenAI}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	_, err := d.Decide(context.Background(), Request{Timeout: 50 * time.Millisecond})
	if err == nil || err.Error() != `invalid adapter_type on router provider "wonky" ("")` {
		t.Errorf("err = %v; want exact routing_trace error string", err)
	}
}

func TestAdapterDecider_NoAdapterForFormat_ErrorTextMatchesTrace(t *testing.T) {
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderName: "exotic-llm",
		Format:       provcore.FormatOpenAI,
	}}
	// AdapterLookup that has zero adapters registered.
	lookup := &fakeAdapterLookup{byFormat: map[provcore.Format]provcore.Adapter{}}
	d := NewAdapterDecider(resolver, lookup, discardLogger())

	_, err := d.Decide(context.Background(), Request{Timeout: 50 * time.Millisecond})
	want := `no adapter for router provider "exotic-llm" (format "openai")`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v; want %q", err, want)
	}
}

func TestAdapterDecider_AdapterReturns500_ErrorTextMatchesTrace(t *testing.T) {
	adapter := &fakeAdapter{
		format:   provcore.FormatOpenAI,
		stubResp: &provcore.Response{StatusCode: 500, Body: []byte(`{"error":"upstream down"}`)},
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderName: "openai", Format: provcore.FormatOpenAI,
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	_, err := d.Decide(context.Background(), Request{Timeout: 50 * time.Millisecond})
	if err == nil || err.Error() != "router LLM error: 500" {
		t.Errorf("err = %v; want %q", err, "router LLM error: 500")
	}
}

func TestAdapterDecider_AdapterTimeout_ErrorTextMatchesTrace(t *testing.T) {
	adapter := &fakeAdapter{
		format: provcore.FormatOpenAI,
		executeFn: func(ctx context.Context, _ provcore.Request) (*provcore.Response, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderName: "openai", Format: provcore.FormatOpenAI,
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	_, err := d.Decide(context.Background(), Request{Timeout: 5 * time.Millisecond})
	if err == nil || err.Error() != "router LLM timeout (5ms)" {
		t.Errorf("err = %v; want %q", err, "router LLM timeout (5ms)")
	}
}

func TestAdapterDecider_AdapterNetworkError_ErrorTextMatchesTrace(t *testing.T) {
	netErr := errors.New("connection refused")
	adapter := &fakeAdapter{
		format:  provcore.FormatOpenAI,
		stubErr: netErr,
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderName: "openai", Format: provcore.FormatOpenAI,
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	_, err := d.Decide(context.Background(), Request{Timeout: 50 * time.Millisecond})
	want := "router LLM error: connection refused"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v; want %q", err, want)
	}
}

// routerDecisionBody is a router-LLM response body that parses to a Decision.
// Token counts do NOT come from here: the decider reads provcore.Response.Usage,
// which the real adapter fills from the provider's own usage block after running
// it through that provider's alias chain. Scripting usage in the body alone
// would test a decoder the decider no longer has.
const routerDecisionBody = `{"choices":[{"message":{"content":"{\"modelId\":\"gpt-4o-mini\",\"reason\":\"cheap\"}"}}]}`

// routerUsageResp builds the adapter response the decider consumes: a parseable
// decision plus the decoded Usage envelope. prompt is the TOTAL input including
// the cached share, matching the canonical normalizer contract.
func routerUsageResp(prompt, completion, cacheRead, cacheCreation int) *provcore.Response {
	u := provcore.Usage{PromptTokens: &prompt, CompletionTokens: &completion}
	if cacheRead > 0 {
		u.CacheReadTokens = &cacheRead
	}
	if cacheCreation > 0 {
		u.CacheCreationTokens = &cacheCreation
	}
	return &provcore.Response{StatusCode: 200, Body: []byte(routerDecisionBody), Usage: u}
}

// openAIRouterRates is gpt-4o list pricing: $2.50/1M input, $10.00/1M output,
// cached input read at 0.25x ($0.625), cache write at the input rate.
var openAIRouterRates = costing.Rates{
	InputUSDPerM: 2.50, OutputUSDPerM: 10.00,
	CacheReadUSDPerM: 0.625, CacheWriteUSDPerM: 2.50,
}

// TestAdapterDecider_Decide_StampsCostAndServingProvider pins the money path:
// the router call's own usage is read off the response, priced from the router
// model's catalog price, and attributed to the provider that SERVED the call.
func TestAdapterDecider_Decide_StampsCostAndServingProvider(t *testing.T) {
	adapter := &fakeAdapter{
		format:   provcore.FormatOpenAI,
		stubResp: routerUsageResp(2000, 50, 0, 0),
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderID:      "prov-openai",
		ProviderName:    "openai",
		Format:          provcore.FormatOpenAI,
		ProviderModelID: "gpt-4o",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())
	d.PriceLookup = func(string) (costing.Rates, bool) { return openAIRouterRates, true }

	got, err := d.Decide(context.Background(), Request{
		RouterProviderID: "prov-openai",
		RouterModelID:    "model-gpt-4o",
		Timeout:          time.Second,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// 2000/1e6*2.50 + 50/1e6*10.00 = 0.005 + 0.0005
	const want = 0.0055
	if math.Abs(got.CostUsd-want) > 1e-9 {
		t.Errorf("CostUsd = %v, want %v", got.CostUsd, want)
	}
	if got.ServedProviderID != "prov-openai" {
		t.Errorf("ServedProviderID = %q, want %q", got.ServedProviderID, "prov-openai")
	}
	if got.PromptTokens != 2000 || got.CompletionTokens != 50 {
		t.Errorf("tokens = (%d,%d), want (2000,50)", got.PromptTokens, got.CompletionTokens)
	}
}

// TestAdapterDecider_Decide_CachedPromptBilledAtCacheRate is the regression
// test for the over-estimate this package shipped: the router's prompt is
// near-identical on every call, so the provider serves most of it from cache
// and reports prompt_tokens INCLUDING that cached share. The old decider read
// only prompt_tokens/completion_tokens off the body and billed all 2000 tokens
// at the $2.50 input rate; the cached 1800 actually bill at $0.625.
//
// The assertion is the exact discounted amount, and separately that it is
// strictly below the full-rate figure — the direction of the old bug.
func TestAdapterDecider_Decide_CachedPromptBilledAtCacheRate(t *testing.T) {
	adapter := &fakeAdapter{
		format:   provcore.FormatOpenAI,
		stubResp: routerUsageResp(2000, 50, 1800, 0),
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderID: "prov-openai", Format: provcore.FormatOpenAI, ProviderModelID: "gpt-4o",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())
	d.PriceLookup = func(string) (costing.Rates, bool) { return openAIRouterRates, true }

	got, err := d.Decide(context.Background(), Request{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if got.CacheReadTokens != 1800 {
		t.Errorf("CacheReadTokens = %d, want 1800 — the adapter's decoded cache bucket must survive", got.CacheReadTokens)
	}
	if got.PromptTokens != 2000 {
		t.Errorf("PromptTokens = %d, want 2000 (the TOTAL input, cached share included)", got.PromptTokens)
	}
	// 200 uncached × 2.50 + 1800 cached × 0.625 + 50 out × 10.00, per 1M.
	want := (200*2.50 + 1800*0.625 + 50*10.00) / 1_000_000.0
	if math.Abs(got.CostUsd-want) > 1e-12 {
		t.Errorf("CostUsd = %v, want %v", got.CostUsd, want)
	}
	// The bug's signature: charging the full input rate on the cached share.
	overEstimate := (2000*2.50 + 50*10.00) / 1_000_000.0
	if got.CostUsd >= overEstimate {
		t.Errorf("CostUsd = %v is not below the full-input-rate figure %v; cached tokens are being billed at the input rate again",
			got.CostUsd, overEstimate)
	}
}

// TestAdapterDecider_Decide_CacheWriteBilledAtWriteRate pins the other cache
// bucket: cache-creation tokens are also a sub-count of prompt_tokens, so they
// must be subtracted from the uncached remainder and billed at the write rate,
// never counted twice.
func TestAdapterDecider_Decide_CacheWriteBilledAtWriteRate(t *testing.T) {
	rates := costing.Rates{
		InputUSDPerM: 3.00, OutputUSDPerM: 15.00,
		CacheReadUSDPerM: 0.30, CacheWriteUSDPerM: 3.75,
	}
	adapter := &fakeAdapter{
		format:   provcore.FormatOpenAI,
		stubResp: routerUsageResp(1000, 20, 200, 700),
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderID: "prov-anthropic", Format: provcore.FormatOpenAI, ProviderModelID: "claude",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())
	d.PriceLookup = func(string) (costing.Rates, bool) { return rates, true }

	got, err := d.Decide(context.Background(), Request{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CacheCreationTokens != 700 {
		t.Errorf("CacheCreationTokens = %d, want 700", got.CacheCreationTokens)
	}
	// 100 uncached × 3.00 + 200 read × 0.30 + 700 write × 3.75 + 20 out × 15.00.
	want := (100*3.00 + 200*0.30 + 700*3.75 + 20*15.00) / 1_000_000.0
	if math.Abs(got.CostUsd-want) > 1e-12 {
		t.Errorf("CostUsd = %v, want %v", got.CostUsd, want)
	}
}

// TestAdapterDecider_Decide_NoPriceLookup_LeavesCostZeroButKeepsProvider pins
// the unpriced-router-model case: no amount, but the vendor attribution must
// still land so the reconciliation report can name who was charged.
func TestAdapterDecider_Decide_NoPriceLookup_LeavesCostZeroButKeepsProvider(t *testing.T) {
	adapter := &fakeAdapter{
		format:   provcore.FormatOpenAI,
		stubResp: routerUsageResp(2000, 50, 0, 0),
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderID: "prov-openai", Format: provcore.FormatOpenAI, ProviderModelID: "gpt-4o",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	got, err := d.Decide(context.Background(), Request{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CostUsd != 0 {
		t.Errorf("CostUsd = %v, want 0 when no PriceLookup is wired", got.CostUsd)
	}
	if got.ServedProviderID != "prov-openai" {
		t.Errorf("ServedProviderID = %q; attribution must survive an unpriced model", got.ServedProviderID)
	}
}

// TestAdapterDecider_Decide_ZeroCatalogPrice_LeavesCostZeroButKeepsProvider
// pins the second unpriced shape: the lookup resolves but the catalog has no
// price for the router model, so the amount stays zero while attribution
// survives. Separate from the nil-lookup case because ok=false is what the
// production lookup emits on a miss.
//
// The token counts must still land: an unpriced router call is exactly the one
// whose spend cannot be seen in the cost column, so its usage is the only
// record that it happened at all.
func TestAdapterDecider_Decide_ZeroCatalogPrice_LeavesCostZeroButKeepsProvider(t *testing.T) {
	adapter := &fakeAdapter{
		format:   provcore.FormatOpenAI,
		stubResp: routerUsageResp(2000, 50, 1800, 0),
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderID: "prov-moonshot", Format: provcore.FormatOpenAI, ProviderModelID: "kimi-k2",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())
	d.PriceLookup = func(string) (costing.Rates, bool) { return costing.Rates{}, false }

	got, err := d.Decide(context.Background(), Request{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CostUsd != 0 {
		t.Errorf("CostUsd = %v, want 0 when the catalog has no price", got.CostUsd)
	}
	if got.ServedProviderID != "prov-moonshot" {
		t.Errorf("ServedProviderID = %q, want prov-moonshot", got.ServedProviderID)
	}
	if got.PromptTokens != 2000 || got.CacheReadTokens != 1800 {
		t.Errorf("tokens = (prompt %d, cacheRead %d), want (2000, 1800); usage must survive an unpriced model",
			got.PromptTokens, got.CacheReadTokens)
	}
}

// TestAdapterDecider_Decide_MissingUsageBlock_StillDecidesAndAttributes pins
// the named failure mode "upstream returned no usage": the routing decision
// already succeeded, so it must NOT fail; tokens and cost stay zero and the
// serving provider is still recorded.
func TestAdapterDecider_Decide_MissingUsageBlock_StillDecidesAndAttributes(t *testing.T) {
	adapter := &fakeAdapter{
		format: provcore.FormatOpenAI,
		stubResp: &provcore.Response{StatusCode: 200, Body: []byte(
			`{"choices":[{"message":{"content":"{\"modelId\":\"gpt-4o-mini\",\"reason\":\"cheap\"}"}}]}`)},
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderID: "prov-openai", Format: provcore.FormatOpenAI, ProviderModelID: "gpt-4o",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())
	d.PriceLookup = func(string) (costing.Rates, bool) { return openAIRouterRates, true }

	got, err := d.Decide(context.Background(), Request{Timeout: time.Second})
	if err != nil {
		t.Fatalf("a missing usage block must not fail a decision that parsed: %v", err)
	}
	if got.ModelID != "gpt-4o-mini" {
		t.Errorf("ModelID = %q, want gpt-4o-mini", got.ModelID)
	}
	if got.PromptTokens != 0 || got.CompletionTokens != 0 || got.CostUsd != 0 {
		t.Errorf("tokens/cost = (%d,%d,%v), want all zero with no usage block",
			got.PromptTokens, got.CompletionTokens, got.CostUsd)
	}
	if got.ServedProviderID != "prov-openai" {
		t.Errorf("ServedProviderID = %q, want prov-openai", got.ServedProviderID)
	}
}

// TestAdapterDecider_Decide_ServedProviderIsNotThePickedProvider is the
// anti-conflation pin: ProviderID carries the provider of the model the router
// PICKED, ServedProviderID the provider that SERVED the router call. They are
// routinely different vendors and must never be sourced from each other.
func TestAdapterDecider_Decide_ServedProviderIsNotThePickedProvider(t *testing.T) {
	adapter := &fakeAdapter{
		format: provcore.FormatOpenAI,
		stubResp: &provcore.Response{StatusCode: 200, Body: []byte(
			`{"choices":[{"message":{"content":"{\"modelId\":\"claude-opus\",\"providerId\":\"prov-anthropic\",\"reason\":\"long ctx\"}"}}],` +
				`"usage":{"prompt_tokens":100,"completion_tokens":10}}`)},
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderID: "prov-openai", Format: provcore.FormatOpenAI, ProviderModelID: "gpt-4o",
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	got, err := d.Decide(context.Background(), Request{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.ProviderID != "prov-anthropic" {
		t.Errorf("ProviderID (picked) = %q, want prov-anthropic", got.ProviderID)
	}
	if got.ServedProviderID != "prov-openai" {
		t.Errorf("ServedProviderID (served) = %q, want prov-openai — the router ran on OpenAI", got.ServedProviderID)
	}
}

func TestAdapterDecider_UnparseableResponse_ErrorTextMatchesTrace(t *testing.T) {
	adapter := &fakeAdapter{
		format:   provcore.FormatOpenAI,
		stubResp: &provcore.Response{StatusCode: 200, Body: []byte("not json")},
	}
	resolver := &fakeResolver{target: provcore.CallTarget{
		ProviderName: "openai", Format: provcore.FormatOpenAI,
	}}
	d := NewAdapterDecider(resolver, newAdapterLookup(adapter), discardLogger())

	_, err := d.Decide(context.Background(), Request{Timeout: 50 * time.Millisecond})
	if err == nil || err.Error() != "failed to parse router response" {
		t.Errorf("err = %v; want %q", err, "failed to parse router response")
	}
}
