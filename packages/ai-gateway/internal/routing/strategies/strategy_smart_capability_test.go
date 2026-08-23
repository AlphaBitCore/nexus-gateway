package strategies

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Capability hard filter: a candidate that DECLARES the relevant list but
// cannot serve what the request provably needs is dropped before the catalog
// is rendered — the router LLM can no longer pick a model that would reject or
// silently ignore the request's images or tools.
//
// Each dimension reads the field that owns it. Image input is a MODALITY and
// is read from InputModalities; function_calling is a capability and is read
// from Features. These rows deliberately give the image cases a model whose
// Features do NOT mention "vision", because that second spelling must no
// longer decide anything — it disagreed with the modality arrays on 34
// production rows.
//
// Candidates with an EMPTY list pass (fail-open: absent catalog metadata must
// not disqualify), and a dimension that would empty the pool is skipped
// entirely (fail-open, trace-recorded).

func imageRctx() *core.RoutingContext {
	return &core.RoutingContext{Request: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "what is in this picture?"},
				{Type: normalize.ContentMedia, MediaRef: &normalize.MediaRef{Modality: normalize.ModalityImage, SizeBytes: 10, Mime: "image/png", SHA256: "a"}},
			}},
		},
	}}
}

func toolsRctx() *core.RoutingContext {
	return &core.RoutingContext{Request: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			textMsg(normalize.RoleUser, "look up the weather"),
		},
		Tools: []normalize.ToolDef{{Name: "get_weather"}},
	}}
}

func TestSmart_CapabilityFilter_ImageRequestDropsNonVisionCandidate(t *testing.T) {
	blind := core.SmartModelRow{ModelID: "m-blind", ModelCode: "text-only", ProviderID: "p1",
		Features: []string{"function_calling", "streaming"}, InputModalities: []string{"text"}}
	// Declares image input and does NOT claim the "vision" feature: the
	// modality array alone must be enough to qualify it.
	sighted := core.SmartModelRow{ModelID: "m-sighted", ModelCode: "sees-all", ProviderID: "p1",
		Features: []string{"streaming"}, InputModalities: []string{"text", "image"}}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "sees-all", Reason: "vision"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{blind, sighted})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(imageRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-sighted" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "text-only") {
		t.Errorf("catalog still contains the vision-less candidate for an image request")
	}
	found := false
	for _, e := range trace {
		if strings.Contains(e.Decision, "capability filter") {
			found = true
		}
	}
	if !found {
		t.Errorf("no capability-filter trace entry: %+v", trace)
	}
}

func TestSmart_CapabilityFilter_ToolsRequestDropsNonFunctionCallingCandidate(t *testing.T) {
	noTools := core.SmartModelRow{ModelID: "m-notools", ModelCode: "chat-only", ProviderID: "p1",
		Features: []string{"streaming"}, InputModalities: []string{"text", "image"}}
	tools := core.SmartModelRow{ModelID: "m-tools", ModelCode: "tool-user", ProviderID: "p1",
		Features: []string{"function_calling"}, InputModalities: []string{"text"}}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "tool-user", Reason: "fc"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{noTools, tools})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(toolsRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-tools" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "chat-only") {
		t.Errorf("catalog still contains the non-function-calling candidate for a tools request")
	}
}

func TestSmart_CapabilityFilter_UndeclaredFeaturesPass(t *testing.T) {
	mystery := core.SmartModelRow{ModelID: "m-mystery", ModelCode: "undeclared", ProviderID: "p1"} // no modalities recorded
	blind := core.SmartModelRow{ModelID: "m-blind", ModelCode: "text-only", ProviderID: "p1",
		Features: []string{"streaming"}, InputModalities: []string{"text"}}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "undeclared", Reason: "only fit"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{mystery, blind})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(imageRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-mystery" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if !strings.Contains(decider.lastReq.SystemPrompt, "undeclared") {
		t.Errorf("undeclared-features candidate must pass the capability filter")
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "text-only") {
		t.Errorf("declared-but-lacking candidate must be dropped")
	}
}

func TestSmart_CapabilityFilter_DimensionEmptyingPoolIsSkipped(t *testing.T) {
	// Every candidate declares its modalities and none accepts image: dropping
	// all would leave the router nothing — the image dimension is skipped
	// (fail-open) and the pool stays intact.
	a := core.SmartModelRow{ModelID: "m-a", ModelCode: "model-a", ProviderID: "p1",
		Features: []string{"streaming"}, InputModalities: []string{"text"}}
	b := core.SmartModelRow{ModelID: "m-b", ModelCode: "model-b", ProviderID: "p1",
		Features: []string{"function_calling"}, InputModalities: []string{"text"}}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "model-a", Reason: "best effort"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{a, b})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(imageRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The subject is the pool the ROUTER saw, asserted below. The plan carries
	// the pick plus what the re-selection pool took, so only position zero is
	// this test's business.
	if len(out) == 0 || out[0].ModelID != "m-a" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if !strings.Contains(decider.lastReq.SystemPrompt, "model-a") || !strings.Contains(decider.lastReq.SystemPrompt, "model-b") {
		t.Errorf("pool must stay intact when the dimension would empty it")
	}
	found := false
	for _, e := range trace {
		if strings.Contains(e.Decision, "no candidate declares") {
			found = true
		}
	}
	if !found {
		t.Errorf("skipped dimension must be trace-recorded: %+v", trace)
	}
}

// The exact production shape, asserted directly: 34 catalog rows advertised
// features ["vision"] while their inputModalities said text only. A router
// that reads the feature list sends the image to a model that cannot read it.
// This row must lose even though its features say it can see.
func TestSmart_CapabilityFilter_VisionFeatureCannotOverrideTextOnlyModalities(t *testing.T) {
	liar := core.SmartModelRow{ModelID: "m-liar", ModelCode: "claims-vision", ProviderID: "p1",
		Features: []string{"vision", "streaming"}, InputModalities: []string{"text"}}
	honest := core.SmartModelRow{ModelID: "m-honest", ModelCode: "really-sees", ProviderID: "p1",
		Features: []string{"streaming"}, InputModalities: []string{"text", "image"}}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "really-sees", Reason: "image input"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{liar, honest})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(imageRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-honest" {
		t.Fatalf("targets = %+v; the vision feature must not qualify a text-only model", out)
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "claims-vision") {
		t.Error("a model advertising vision with text-only input reached the router catalog")
	}
}
