package routing

// Two directions this filter must NOT take, both found by review after the
// first version shipped its comment claiming neither was possible.

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func mediaRequest(model, modality string) *core.RoutingContext {
	return &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: model},
		EndpointType:   "chat",
		Request: &normcore.NormalizedPayload{
			Messages: []normcore.Message{{
				Role: "user",
				Content: []normcore.ContentBlock{
					{Text: "what does this say?"},
					{MediaRef: &normcore.MediaRef{Modality: modality, Source: normcore.MediaCaptured}},
				},
			}},
		},
	}
}

// A modality NO model in the catalog declares carries no evidence about any
// individual model, so it cannot be grounds for refusing one.
//
// Measured on tools/db-migrate/model-catalog.json: 163 chat rows, of which 163
// declare text, 88 image, 2 audio, and ZERO declare file or video. A document
// request therefore fails the "does this model accept what we carry" test
// against every row — and a filter that takes that at face value deletes the
// whole recovery list. filterByCapability has carried an escape for exactly
// this since it was written; its comment names documents by name. This one
// shipped without.
func TestDocumentRequest_DoesNotEmptyTheRecoveryList(t *testing.T) {
	for _, modality := range []string{"file", "video"} {
		t.Run(modality, func(t *testing.T) {
			got := dispatchOrder(t, audioFixture(t), mediaRequest("auto", modality))
			if len(got) < 3 {
				t.Errorf("a %s request left %d of 3 targets: %v\n"+
					"  no model in this catalog declares %s, so its absence on a row says "+
					"nothing about that row — dropping every candidate turns an attachment "+
					"we could have tried into a routing failure", modality, len(got), got, modality)
			}
		})
	}
}

// The ceiling may be skipped when the request carries nothing; the FLOOR may
// not.
//
// CarriedModalities returns only MediaRef modalities, so a plain-text request
// yields an empty set — and a filter gated on that set being non-empty skips
// the floor too. gpt-audio-mini REQUIRES audio; the smart strategy grew its own
// floor because that model "lands in the pool for every plain text request and
// then 400s upstream, sixteen times before this existed". The recovery list had
// no such protection: on a text request it survives and serves on failover.
func TestTextOnlyRequest_DropsARecoveryTargetThatRequiresAudio(t *testing.T) {
	f := audioFixture(t)
	// Give the audio-floored model a recovery slot and a primary that can
	// serve plain text, which is the shape a text request actually routes into.
	f.addChatModel("plain-chat", "openai", []string{"text"})
	f.store.models["gpt-audio-mini"].RequiredModalities = []string{"audio"}
	f.store.rules[0].Config = mustJSONCapability(t, map[string]any{
		"type": "single", "providerId": "openai", "modelId": "plain-chat",
	})
	f.store.rules[0].FallbackChain = mustJSONCapability(t, []map[string]any{
		{"providerId": "openai", "modelId": "gpt-audio-mini"},
	})
	f.rebuildCapCache()

	rctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		EndpointType:   "chat",
		Request: &normcore.NormalizedPayload{
			Messages: []normcore.Message{{Role: "user",
				Content: []normcore.ContentBlock{{Text: "hello"}}}},
		},
	}
	for _, id := range dispatchOrder(t, f, rctx) {
		if id == "gpt-audio-mini" {
			t.Errorf("gpt-audio-mini is in the dispatch list for a text-only request "+
				"and REQUIRES audio.\n"+
				"  it reaches the executor because the filter is gated on the request "+
				"carrying a modality, and a text request carries none — so the floor, "+
				"which is about what the MODEL demands, never runs.\n"+
				"  dispatch order: %v", dispatchOrder(t, f, rctx))
		}
	}
	_ = context.Background
}

// The line between whose choice it is, asserted from the side that is easy to
// lose: when the CALLER names a model, we do not filter anything.
//
// A caller who writes `model: moonshot-v1-128k` owns that model's limits, and
// the admin who configured its fallback chain owns that too. The upstream's own
// refusal is the honest answer and it is the one they should get; standing in
// front of it turns a provider's message into ours, on a decision neither of
// them delegated.
//
// This is the direction with no natural alarm. Filtering too much looks like
// working code — every other test in this package still passes when the filter
// is widened to every request, which is how it got written that way the first
// time.
func TestCallerNamedModel_RecoveryIsForwardedUnfiltered(t *testing.T) {
	f := audioFixture(t)
	named := mediaRequest("gpt-audio-mini", normcore.ModalityAudio)
	got := dispatchOrder(t, f, named)

	if len(got) != 3 {
		t.Errorf("dispatch order = %v, want all three targets kept.\n"+
			"  the caller named gpt-audio-mini, so the fallbacks an admin configured "+
			"behind it are their choice too — a model that cannot read the attachment "+
			"answers for itself, and that answer belongs to the upstream, not to us", got)
	}
}

// The primary is NOT filtered here, and that has to be pinned from both sides
// in one assertion: extending this filter to plan.Targets is the documented
// fail-closed hazard, and nothing could see it.
//
// The primary belongs to whichever strategy produced it — smart applies its own
// capability filter, the others are an admin's explicit pick. Re-filtering here
// would either duplicate that work or overrule a deliberate configuration, and
// on a plan whose primary is the only target it would empty the dispatch list
// outright.
func TestRecoveryFilter_LeavesThePrimaryAlone(t *testing.T) {
	f := newCapFixture()
	f.addProvider("openai", "openai", true)
	f.addProvider("moonshot", "openai", true)
	f.addChatModel("text-primary", "openai", []string{"text"})
	f.addChatModel("audio-capable", "openai", []string{"text", "audio"})
	f.addChatModel("moonshot-v1-128k", "moonshot", []string{"text"})
	f.rebuildCapCache()

	f.addRule(store.RoutingRule{
		ID: "r-primary", PipelineStage: 1, StrategyType: "single",
		Config: mustJSONCapability(t, map[string]any{
			"type": "single", "providerId": "openai", "modelId": "text-primary",
		}),
		FallbackChain: mustJSONCapability(t, []map[string]any{
			{"providerId": "openai", "modelId": "audio-capable"},
			{"providerId": "moonshot", "modelId": "moonshot-v1-128k"},
		}),
	})

	got := dispatchOrder(t, f, audioRequest("auto"))

	var sawPrimary, sawIncapableRecovery bool
	for _, id := range got {
		switch id {
		case "text-primary":
			sawPrimary = true
		case "moonshot-v1-128k":
			sawIncapableRecovery = true
		}
	}
	if !sawPrimary {
		t.Errorf("the primary text-primary was filtered out: %v\n"+
			"  this filter must never touch plan.Targets — on a plan whose primary is "+
			"the only target, doing so empties the dispatch list and the request 404s", got)
	}
	if sawIncapableRecovery {
		t.Errorf("moonshot-v1-128k survived in recovery for an audio request: %v", got)
	}
}
