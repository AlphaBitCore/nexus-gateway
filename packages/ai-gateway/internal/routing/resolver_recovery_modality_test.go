package routing

// The recovery list has to clear the same capability bar as the primary, and
// every assertion in this file is on the list the EXECUTOR walks —
// ResolveTargets' RouteResult.Targets — not on plan.RecoveryTargets.
//
// That distinction is the whole point. The filter runs inside Resolve; three
// things happen to its output afterwards (flatten, modality guard, health
// reorder), and a plan-level assertion is blind to all three. This program has
// already shipped a defect whose plan said one model and whose dispatch was
// another, so a test that stops at the plan is a test that cannot see the bug
// class it was written for.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// addChatModel registers a chat model declaring exactly these input modalities.
// A nil list means the catalog says nothing, which is the undeclared case and
// must NOT read as "text only".
func (f *capFixture) addChatModel(id, providerID string, inputModalities []string) {
	f.store.models[id] = &store.Model{
		ID: id, Code: id, Name: id,
		ProviderID: providerID, ProviderName: providerID, ProviderModelID: id,
		Type: "chat", Enabled: true,
		InputModalities:  inputModalities,
		OutputModalities: []string{"text"},
		Lifecycle:        "ga",
	}
}

// audioRequest builds a routing context for a chat request carrying one audio
// attachment, in the shape normalization produces for every ingress.
//
// The model is "auto" throughout this file, and that is load-bearing rather
// than incidental: the filter under test only runs when the CALLER delegated
// the choice. A caller who names a model owns its limits — we forward and let
// the upstream answer. Written with a named model first, these tests passed
// against a resolver that never filtered anything.
func audioRequest(model string) *core.RoutingContext {
	return &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: model},
		EndpointType:   "chat",
		Request: &normcore.NormalizedPayload{
			Messages: []normcore.Message{{
				Role: "user",
				Content: []normcore.ContentBlock{
					{Text: "Transcribe the attached audio exactly."},
					{MediaRef: &normcore.MediaRef{
						Modality: normcore.ModalityAudio,
						Mime:     "audio/wav",
						Source:   normcore.MediaCaptured,
					}},
				},
			}},
		},
	}
}

// dispatchOrder returns the model IDs in the order the executor would try them.
func dispatchOrder(t *testing.T, f *capFixture, rctx *core.RoutingContext) []string {
	t.Helper()
	res, err := f.resolver.ResolveTargets(context.Background(), rctx)
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	out := make([]string, 0, len(res.AllTargets()))
	for _, tg := range res.AllTargets() {
		out = append(out, tg.ModelID)
	}
	return out
}

// audioFixture wires the shape production was in when this was found: an
// audio-capable primary, one audio-incapable model reachable through the
// primary rule's inline FallbackChain, and a second reachable through a
// separate fallback RULE.
//
// Both recovery paths are populated on purpose. They are built by different
// code — the inline chain unmarshals FallbackChain and looks each entry up, the
// fallback rule runs a whole strategy — and they converge on the same
// plan.RecoveryTargets slice. A fix applied to one and not the other looks
// complete from either path alone.
func audioFixture(t *testing.T) *capFixture {
	t.Helper()
	f := newCapFixture()
	f.addProvider("openai", "openai", true)
	f.addProvider("moonshot", "openai", true)

	f.addChatModel("gpt-audio-mini", "openai", []string{"text", "audio"})
	f.addChatModel("moonshot-v1-128k", "moonshot", []string{"text"})
	f.addChatModel("moonshot-v1-32k", "moonshot", []string{"text"})
	f.rebuildCapCache()

	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		PipelineStage: 1,
		StrategyType:  "single",
		Config: mustJSONCapability(t, map[string]any{
			"type": "single", "providerId": "openai", "modelId": "gpt-audio-mini",
		}),
		FallbackChain: mustJSONCapability(t, []map[string]any{
			{"providerId": "moonshot", "modelId": "moonshot-v1-128k"},
		}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-fallback",
		PipelineStage: 1,
		StrategyType:  "fallback",
		Config: mustJSONCapability(t, map[string]any{
			"type": "single", "providerId": "moonshot", "modelId": "moonshot-v1-32k",
		}),
	})
	return f
}

// The named failure, asserted where it was felt.
//
// Measured in production: `model: auto` with a WAV attachment traced
// "capability filter: dropped 44/51 candidates" and "selected gpt-audio-mini",
// then answered with routed_model_name = moonshot-v1-128k — a model whose
// catalog row declares text only. The caller got a refusal about audio from a
// model that was never eligible to be asked, and the trace said we knew.
func TestAudioRequest_NoDispatchTargetIsIncapableOfAudio(t *testing.T) {
	got := dispatchOrder(t, audioFixture(t), audioRequest("auto"))

	for _, id := range got {
		if id == "moonshot-v1-128k" || id == "moonshot-v1-32k" {
			t.Errorf("%s is in the DISPATCH list for an audio request and declares text only.\n"+
				"  dispatch order: %v\n"+
				"  it reaches the executor through recovery, which the capability filter "+
				"did not cover — the primary was filtered and the fallback was not", id, got)
		}
	}
	if len(got) == 0 || got[0] != "gpt-audio-mini" {
		t.Fatalf("dispatch order = %v; the audio-capable primary must lead it.\n"+
			"  an empty list would mean the filter emptied the plan — fail-closed, "+
			"which this must never do", got)
	}
}

// The fail-open direction, and the reason the filter reads InputModalities
// rather than a feature flag: a model the catalog says NOTHING about is not a
// text-only model. Reading an empty list as a restriction is what hid 36
// document-capable models from routing until the catalog was corrected, and a
// filter that repeats that mistake takes capability away with nothing in the
// error to explain it.
func TestAudioRequest_UndeclaredModalitiesSurviveRecovery(t *testing.T) {
	f := audioFixture(t)
	f.addChatModel("mystery-model", "moonshot", nil) // catalog says nothing
	f.store.rules[1].Config = mustJSONCapability(t, map[string]any{
		"type": "single", "providerId": "moonshot", "modelId": "mystery-model",
	})
	f.rebuildCapCache()

	got := dispatchOrder(t, f, audioRequest("auto"))
	found := false
	for _, id := range got {
		if id == "mystery-model" {
			found = true
		}
	}
	if !found {
		t.Errorf("mystery-model was dropped from recovery: %v\n"+
			"  it declares no input modalities, which means UNDECLARED, not text-only", got)
	}
}

// A request carrying no media must not be filtered at all. The filter's cost
// and its blast radius both belong to requests that actually carry something;
// a text request that loses its fallbacks pays for a check that could not have
// had an opinion.
func TestTextOnlyRequest_RecoveryIsUntouched(t *testing.T) {
	f := audioFixture(t)
	rctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-audio-mini"},
		EndpointType:   "chat",
		Request: &normcore.NormalizedPayload{
			Messages: []normcore.Message{{
				Role:    "user",
				Content: []normcore.ContentBlock{{Text: "hello"}},
			}},
		},
	}
	got := dispatchOrder(t, f, rctx)
	want := []string{"gpt-audio-mini", "moonshot-v1-128k", "moonshot-v1-32k"}
	// The whole ordered slice, not its length. A bare count cannot see a
	// primary/recovery order inversion at the flatten, an identity swap in the
	// FallbackChain lookup, or a substitution — three mutations that all leave
	// exactly three targets standing and were each green against `len(got) != 3`.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dispatch order = %v, want %v — a text request carries no modality "+
			"the filter can have an opinion about, so the list must arrive intact "+
			"AND in order, primary first", got, want)
	}
}

// The trace has to say so. A drop that leaves no record turns a "why did my
// fallback not run" question into an unanswerable one, and every other stage
// that removes a target here (modality guard, capability pre-filter) records
// its own.
func TestAudioRequest_DroppedRecoveryTargetsAreTraced(t *testing.T) {
	f := audioFixture(t)
	// Read the trace off ResolveTargets' RouteResult, which is what
	// buildRoutingAuditTrace stores in traffic_event.routing_trace. Asserted on
	// the PLAN first, this passed while a mutation dropping `Trace:` from the
	// RouteResult literal erased the record from every audit row.
	res, err := f.resolver.ResolveTargets(context.Background(), audioRequest("auto"))
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	var found bool
	for _, e := range res.Trace {
		if e.StrategyType == "recovery" &&
			strings.Contains(e.Decision, "capability filter") &&
			strings.Contains(e.Decision, "audio") {
			found = true
		}
	}
	if !found {
		t.Errorf("no trace entry in the AUDITED trace names the capability drop and "+
			"the modality that caused it.\n  trace: %+v", res.Trace)
	}
	// The count too: a message that says "dropped 1" when it dropped 2 sends
	// whoever reads it looking for a target that is not there.
	if !strings.Contains(traceText(res.Trace), "dropped 2 recovery target(s)") {
		t.Errorf("the trace does not report how many were dropped (2 here):\n%s",
			traceText(res.Trace))
	}
}

func traceText(entries []core.TraceEntry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.StrategyType)
		b.WriteString(": ")
		b.WriteString(e.Decision)
		b.WriteString("\n")
	}
	return b.String()
}

// addFlooredChatModel registers a chat model that REQUIRES a modality: a
// request carrying none of them cannot be served by it at all.
func (f *capFixture) addFlooredChatModel(id, providerID string, input, required []string) {
	f.addChatModel(id, providerID, input)
	f.store.models[id].RequiredModalities = required
}

// textRequest is the plain-text counterpart to audioRequest: a chat request
// carrying nothing but text.
func textRequest(model string) *core.RoutingContext {
	return &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: model},
		EndpointType:   "chat",
		Request: &normcore.NormalizedPayload{
			Messages: []normcore.Message{{
				Role:    "user",
				Content: []normcore.ContentBlock{{Text: "hello"}},
			}},
		},
	}
}

// TestFloor_NamedModelIsCallerOwnedUnlessEnforced is the #297 policy split.
//
// The required-modality floor is a real property of the model — a text-only
// request sent to a model that REQUIRES audio cannot succeed. But who owns that
// verdict depends on who chose the model, and the catalogue that supplies the
// floor has been wrong (mislabelled video, reasoning), so the policy is:
//
//   - `auto` (the GATEWAY chose): the floor holds unconditionally — a target we
//     pick must be one the executor can serve. Both flag values.
//   - a NAMED model (here a `single` rule's explicit target, callerNamedTheModel
//     = true): by default the floor is deferred to the upstream — the caller
//     owns the model they named, and our floor data can be wrong. Enforced only
//     when EnforceNamedModelModality is on.
//
// The `auto` cases are the control that proves the flag never touches a
// gateway-chosen model; the two named cases are the red-proof that the flag,
// and only the flag, moves the named verdict. Removing the gate (reverting the
// floor to unconditional) makes the enforce=false named case filter and fail
// here — which is the specific violation this guards.
func TestFloor_NamedModelIsCallerOwnedUnlessEnforced(t *testing.T) {
	build := func(enforce bool) *capFixture {
		f := newCapFixture()
		f.resolver.enforceNamedModelModality = enforce
		f.addProvider("openai", "openai", true)
		// Requires audio and accepts text — the real shape of gpt-audio-mini,
		// which is why a text request reaches it at all: its TYPE is chat.
		f.addFlooredChatModel("gpt-audio-mini", "openai", []string{"text", "audio"}, []string{"audio"})
		f.rebuildCapCache()
		f.addRule(store.RoutingRule{
			ID:            "r-primary",
			PipelineStage: 1,
			StrategyType:  "single",
			Config: mustJSONCapability(t, map[string]any{
				"type": "single", "providerId": "openai", "modelId": "gpt-audio-mini",
			}),
		})
		return f
	}
	routedToFloored := func(got []string) bool {
		for _, id := range got {
			if id == "gpt-audio-mini" {
				return true
			}
		}
		return false
	}

	// `auto`: the gateway chose, so the floor holds regardless of the flag.
	for _, enforce := range []bool{false, true} {
		f := build(enforce)
		if routedToFloored(dispatchOrder(t, f, textRequest("auto"))) {
			t.Errorf("enforce=%v: `auto` routed a text-only request to gpt-audio-mini, which "+
				"REQUIRES audio — the gateway chose the model and must not hand the executor a "+
				"target it knows cannot serve", enforce)
		}
	}

	// NAMED + flag OFF (default): fail-open. The caller owns the model, and our
	// floor data can be wrong, so the request reaches the target and the upstream
	// gives its own verdict rather than ours.
	if !routedToFloored(dispatchOrder(t, build(false), textRequest("gpt-audio-mini"))) {
		t.Error("enforce=false: a NAMED gpt-audio-mini was floor-filtered, but the default is to " +
			"defer a named model's modality verdict to the upstream")
	}

	// NAMED + flag ON: the floor is enforced for named targets too.
	if routedToFloored(dispatchOrder(t, build(true), textRequest("gpt-audio-mini"))) {
		t.Error("enforce=true: a NAMED gpt-audio-mini was NOT floor-filtered, but " +
			"EnforceNamedModelModality should enforce the floor for named targets")
	}
}

// The other direction, and the reason this is a FLOOR filter and not a
// capability filter: a request that DOES carry the required modality must still
// reach the model. A filter that dropped it would take the audio traffic away
// from the only model that can serve it.
func TestFloor_AMetFloorStillReachesTheModel(t *testing.T) {
	f := newCapFixture()
	f.addProvider("openai", "openai", true)
	f.addFlooredChatModel("gpt-audio-mini", "openai", []string{"text", "audio"}, []string{"audio"})
	f.rebuildCapCache()
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		PipelineStage: 1,
		StrategyType:  "single",
		Config: mustJSONCapability(t, map[string]any{
			"type": "single", "providerId": "openai", "modelId": "gpt-audio-mini",
		}),
	})

	got := dispatchOrder(t, f, audioRequest("auto"))
	if len(got) == 0 || got[0] != "gpt-audio-mini" {
		t.Fatalf("dispatch order = %v; an audio request meets this model's floor and must "+
			"reach it — a floor filter that also drops the requests it was written for is "+
			"just an outage", got)
	}
}
