package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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
func (a *fakeAdapter) PrepareBody(req provcore.Request) ([]byte, []string, error) {
	return req.Body, nil, nil
}
func (a *fakeAdapter) ExecuteWithBody(ctx context.Context, req provcore.Request, body []byte, _ []string) (*provcore.Response, error) {
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
