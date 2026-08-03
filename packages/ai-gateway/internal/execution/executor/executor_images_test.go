package executor

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func geminiImageResolver() provtargetFunc {
	return provtargetFunc(func(_ context.Context, _, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
		return provcore.CallTarget{
			ProviderName:    "gemini-target",
			Format:          provcore.FormatGemini,
			BaseURL:         "https://generativelanguage.googleapis.com",
			APIKey:          "k",
			ProviderModelID: "gemini-2.5-flash-image",
		}, nil
	})
}

// TestExecute_ImagesBridgeRewrites_ReachAdapter is the executor-seam
// assertion for the cross-shape image leg: an OpenAI-images request
// routed to a Gemini target is translated by the bridge, the call-time
// wire shape flips to the Gemini image leg, and the codec's coercion
// rewrites (size→aspectRatio, quality drop, response_format default)
// arrive at adapter.ExecuteWithBody — this is what keeps the
// x-nexus-coerced contract alive on bridge-translated (failover) legs,
// where the prepare stage's rewrite plumbing never ran.
func TestExecute_ImagesBridgeRewrites_ReachAdapter(t *testing.T) {
	gem := &mockAdapter{format: provcore.FormatGemini, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`{"created":1,"data":[{"b64_json":"QUFB"}]}`)}},
	}}
	reg := provcore.NewRegistry()
	if err := reg.Register(gem); err != nil {
		t.Fatalf("register gem: %v", err)
	}
	reg.Freeze()

	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(slog.Default()))
	exec := New(reg, geminiImageResolver(), nil, bridge)

	base := provcore.Request{
		WireShape:  typology.WireShapeOpenAIImages,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"img-alias","prompt":"a fox","size":"1792x1024","quality":"hd"}`),
	}
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("gem")},
		base,
		configtypes.DefaultRetryPolicy(),
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v (attempts=%+v)", result.Error, result.Attempts)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}
	if gem.executeWithBodyCalls != 1 {
		t.Fatalf("expected ExecuteWithBody once (bridge-rewrites path), got %d", gem.executeWithBodyCalls)
	}
	// The call-time wire shape must be the Gemini image leg (BuildURL +
	// codec dispatch key), never the caller's openai-images ingress shape.
	if gem.lastReq.WireShape != typology.WireShapeGeminiImagesGenerateContent {
		t.Fatalf("wire shape at adapter = %v, want gemini image leg", gem.lastReq.WireShape)
	}
	if got := gjson.GetBytes(gem.lastBody, "contents.0.parts.0.text").String(); got != "a fox" {
		t.Fatalf("translated wire body prompt = %q", got)
	}
	joined := strings.Join(gem.lastRewrites, ",")
	for _, want := range []string{"size:1792x1024→aspectRatio 16:9", "quality→dropped", "response_format:default→b64_json"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rewrites at adapter %v missing %q", gem.lastRewrites, want)
		}
	}
}

// TestExecute_ImagesPreparedRetry_KeepsRewrites pins the retry arm of
// the coercion contract: when the primary target's first attempt fails
// retryably and the SAME prepared body is re-dispatched, the prepare
// stage's rewrites must still reach the adapter — a coerced image
// request retried after a transient failure must not silently lose its
// x-nexus-coerced markers.
func TestExecute_ImagesPreparedRetry_KeepsRewrites(t *testing.T) {
	gem := &mockAdapter{format: provcore.FormatGemini, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 500, Body: []byte(`upstream hiccup`)}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`{"created":1,"data":[{"b64_json":"QUFB"}]}`)}},
	}}
	reg := provcore.NewRegistry()
	if err := reg.Register(gem); err != nil {
		t.Fatalf("register gem: %v", err)
	}
	reg.Freeze()

	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(slog.Default()))
	exec := New(reg, geminiImageResolver(), nil, bridge)

	base := provcore.Request{
		WireShape:  typology.WireShapeGeminiImagesGenerateContent,
		BodyFormat: provcore.FormatGemini,
		Body:       []byte(`{"contents":[{"role":"user","parts":[{"text":"a fox"}]}]}`),
	}
	rewrites := []string{"size:1792x1024→aspectRatio 16:9", "response_format:default→b64_json"}
	result := exec.ExecuteWithPreparedBody(context.Background(),
		[]routingcore.RoutingTarget{target("gem")},
		base,
		configtypes.DefaultRetryPolicy(),
		base.Body, rewrites, "",
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v (attempts=%+v)", result.Error, result.Attempts)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", result.StatusCode)
	}
	if gem.executeWithBodyCalls != 2 {
		t.Fatalf("both primary attempts must use the prepared body; ExecuteWithBody calls = %d", gem.executeWithBodyCalls)
	}
	if strings.Join(gem.lastRewrites, ",") != strings.Join(rewrites, ",") {
		t.Fatalf("retried attempt lost the prepared rewrites: %v", gem.lastRewrites)
	}
	// (Response.Coerced surfacing from those rewrites is the real
	// adapter's job — the mock returns scripted responses — so the
	// business assertion here is that the rewrites REACH the adapter on
	// the retry attempt.)
}

// TestExecute_ImagesTranslateError_NeverReachesAdapter pins the
// failover-lane validation: a body that violates the images closed set
// (n over the fan-out cap) fails the bridge translation as an attempt
// error — the Gemini adapter is never invoked, so the invalid request
// can never reach the paid upstream via a bridge-translated leg.
func TestExecute_ImagesTranslateError_NeverReachesAdapter(t *testing.T) {
	gem := &mockAdapter{format: provcore.FormatGemini}
	reg := provcore.NewRegistry()
	if err := reg.Register(gem); err != nil {
		t.Fatalf("register gem: %v", err)
	}
	reg.Freeze()

	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(slog.Default()))
	exec := New(reg, geminiImageResolver(), nil, bridge)

	base := provcore.Request{
		WireShape:  typology.WireShapeOpenAIImages,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"img-alias","prompt":"a fox","n":50}`),
	}
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("gem")},
		base,
		configtypes.DefaultRetryPolicy(),
	)
	if result.Error == nil {
		t.Fatal("expected all-targets-exhausted error")
	}
	if len(result.Attempts) != 1 || !contains(result.Attempts[0].Error, "hub translate") {
		t.Fatalf("want one translate-error attempt, got %+v", result.Attempts)
	}
	if !contains(result.Attempts[0].Error, `"n"`) {
		t.Fatalf("attempt error %q must name the rejected field", result.Attempts[0].Error)
	}
	if gem.executeWithBodyCalls != 0 || gem.callIndex != 0 {
		t.Fatal("invalid images body must never reach the Gemini adapter")
	}
}
