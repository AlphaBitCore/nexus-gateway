package matcher

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestModelTypes_MatchesTheEndpointNotTheNamedModelsCatalogueRow.
//
// A rule saying "this applies to embedding traffic" is written for embedding
// traffic. It used to be compared against the catalogue type of whatever model
// the caller named — and an `auto` request names no model, so the field was
// empty, the comparison failed, and the rule silently never matched the
// requests it most obviously exists for. The admin sees a rule that is enabled,
// correct-looking, and inert.
//
// The endpoint is a fact of the request and is always present. The stored
// values stay as they are: `EndpointKindAcceptsModelType` is the one place the
// kind question is answered, and it already translates `embedding` to the
// embeddings endpoint, so nothing has to be migrated for an admin's existing
// rules to keep meaning what they meant.
func TestModelTypes_MatchesTheEndpointNotTheNamedModelsCatalogueRow(t *testing.T) {
	conds := &core.MatchConditions{ModelTypes: []string{"embedding"}}

	auto := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		EndpointType:   typology.EndpointKindEmbeddings,
	}
	if !RuleMatchesContext(conds, "auto", auto) {
		t.Error("an embedding-typed rule did not match an `auto` request on the embeddings " +
			"endpoint — the rule is enabled, reads correctly, and never fires")
	}

	// The condition still constrains. A chat request must not pick up a rule
	// written for embeddings just because the rule stopped reading the model.
	chat := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		EndpointType:   typology.EndpointKindChat,
	}
	if RuleMatchesContext(conds, "auto", chat) {
		t.Error("an embedding-typed rule matched a chat request — the condition stopped " +
			"constraining anything and now every rule is a catch-all")
	}

	// A named model does not change the answer: the endpoint decides.
	namedChat := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "m1", Type: "embedding", ProviderID: "p1"},
		EndpointType:   typology.EndpointKindChat,
	}
	if RuleMatchesContext(conds, "m1", namedChat) {
		t.Error("the rule matched on the named model's catalogue row while the request was " +
			"on a different endpoint — the model's type is not what the rule is about")
	}
}

// TestProviders_IsInapplicableWhenTheCallerNamedNoModel.
//
// `providers` asks whether the model the caller named belongs to a provider
// this rule handles. An `auto` request names no model, so the question has no
// answer — and reading the absent answer as "no" made a provider-only rule
// invisible to exactly the requests it exists for. An admin who selects one
// provider means "route within this provider", and `auto` is the case where we
// do the routing.
//
// Inapplicable, not true. A request that DID name a model is still compared, so
// a rule scoped to one provider still leaves another provider's model alone —
// which is the half that keeps this from becoming a catch-all.
func TestProviders_IsInapplicableWhenTheCallerNamedNoModel(t *testing.T) {
	conds := &core.MatchConditions{Providers: []string{"anthropic"}}

	auto := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		EndpointType:   typology.EndpointKindChat,
	}
	if !RuleMatchesContext(conds, "auto", auto) {
		t.Error("a provider-scoped rule did not match an `auto` request — the admin asked to " +
			"route within one provider and the rule is invisible to every request that asks " +
			"us to route")
	}

	named := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4o", ProviderID: "openai"},
		EndpointType:   typology.EndpointKindChat,
	}
	if RuleMatchesContext(conds, "gpt-4o", named) {
		t.Error("a rule scoped to anthropic matched a request naming an openai model — " +
			"inapplicability was read as 'always true' and the condition constrains nothing")
	}

	sameProvider := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "claude", ProviderID: "anthropic"},
		EndpointType:   typology.EndpointKindChat,
	}
	if !RuleMatchesContext(conds, "claude", sameProvider) {
		t.Error("a rule scoped to anthropic did not match a request naming an anthropic model")
	}
}

// TestProviders_ComparesEveryProviderServingTheNamedCode.
//
// A code two providers both serve is ordinary — the same open-weights model on
// two hosts — and the catalogue query that resolves it states no order. So
// comparing one of the candidates makes the rule fire according to whichever
// row Postgres returned first: two identical requests, two answers, and nothing
// on either recording which provider the comparison used.
//
// Both orders are asserted because ONE order lands on the right answer by
// accident, which is how this stayed green while the defect was live.
func TestProviders_ComparesEveryProviderServingTheNamedCode(t *testing.T) {
	conds := &core.MatchConditions{Providers: []string{"together"}}

	for _, tc := range []struct {
		name  string
		order []string
	}{
		{"together first", []string{"together", "groq"}},
		{"groq first", []string{"groq", "together"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &core.RoutingContext{
				RequestedModel: core.RequestedModel{
					ID:                   "llama-3.3-70b",
					CandidateIDs:         []string{"m-a", "m-b"},
					CandidateProviderIDs: tc.order,
				},
				EndpointType: typology.EndpointKindChat,
			}
			if !RuleMatchesContext(conds, "llama-3.3-70b", ctx) {
				t.Fatalf("a rule scoped to together did not match a code together serves — "+
					"the answer came from catalogue row order (%v), so the same request "+
					"routes differently on a different day", tc.order)
			}
		})
	}

	// The half that keeps this from becoming a catch-all: a code NO listed
	// provider serves still fails the condition.
	other := &core.RoutingContext{
		RequestedModel: core.RequestedModel{
			ID: "gpt-4o", CandidateIDs: []string{"m-c"},
			CandidateProviderIDs: []string{"openai"},
		},
		EndpointType: typology.EndpointKindChat,
	}
	if RuleMatchesContext(conds, "gpt-4o", other) {
		t.Error("a rule scoped to together matched a model only openai serves")
	}
}

// TestRequestedModelLiterals_MatchTheKeywordsTheFormAdvertises.
//
// The field is a list of keywords compared against the RAW `model` string,
// before the catalogue resolves it — which is where version suffixes live, so
// a pattern is the only way to write a rule that survives the next model
// release. The admin form offers `gpt-4-*` as its example and always has; the
// comparison was exact, so a rule written from that example matched nothing
// and left nothing to read.
func TestRequestedModelLiterals_MatchTheKeywordsTheFormAdvertises(t *testing.T) {
	conds := &core.MatchConditions{RequestedModelLiterals: []string{"gpt-4o-*"}}
	ctx := func(model string) *core.RoutingContext {
		return &core.RoutingContext{
			RequestedModel: core.RequestedModel{ID: model},
			EndpointType:   typology.EndpointKindChat,
		}
	}

	if !RuleMatchesContext(conds, "gpt-4o-2024-11-20", ctx("gpt-4o-2024-11-20")) {
		t.Error("a dated release of the model the pattern names did not match — every rule an " +
			"admin wrote from the form's own example is inert, and the form still says it works")
	}
	// The other half: a keyword list is still a filter, not a catch-all.
	if RuleMatchesContext(conds, "claude-sonnet-4-5", ctx("claude-sonnet-4-5")) {
		t.Error("a model the pattern does not name matched; the condition constrains nothing")
	}
	// A literal with no wildcard keeps comparing exactly. Every smart rule is
	// required to pin `auto`, so this is the case that must not move.
	autoOnly := &core.MatchConditions{RequestedModelLiterals: []string{"auto"}}
	if !RuleMatchesContext(autoOnly, "auto", ctx("auto")) {
		t.Error("`auto` stopped matching itself — smart rules are required to pin it")
	}
	if RuleMatchesContext(autoOnly, "automatic", ctx("automatic")) {
		t.Error("`auto` matched `automatic`; a literal without a wildcard must stay a literal, " +
			"or every smart rule silently widens to models whose names merely start with it")
	}
}

// TestProviders_RefusesWhenTheCatalogueCouldNotAnswer.
//
// Three states, not two. "Named nothing" makes the condition INAPPLICABLE —
// an `auto` request is exactly what a provider-scoped rule is written for.
// "Named something we could not look up" is neither: the condition cannot be
// evaluated, and a condition that cannot be evaluated has not passed.
//
// Collapsing the second into the first meant that during a catalogue read
// failure a rule scoped to one provider matched models on every OTHER provider
// and redirected them there — fail-open in the one direction that moves traffic
// somewhere the admin excluded.
func TestProviders_RefusesWhenTheCatalogueCouldNotAnswer(t *testing.T) {
	conds := &core.MatchConditions{Providers: []string{"anthropic"}}

	unknowable := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4o", HydrationFailed: true},
		EndpointType:   typology.EndpointKindChat,
	}
	if RuleMatchesContext(conds, "gpt-4o", unknowable) {
		t.Error("a rule scoped to anthropic matched a request whose providers we could not " +
			"resolve — during a catalogue blip every provider-scoped rule widens to every provider")
	}

	// The inapplicable case is untouched: `auto` names nothing, and the rule
	// exists for exactly those requests.
	auto := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		EndpointType:   typology.EndpointKindChat,
	}
	if !RuleMatchesContext(conds, "auto", auto) {
		t.Error("an `auto` request stopped matching a provider-scoped rule; inapplicable was " +
			"turned into false, which makes the rule invisible to the traffic it is written for")
	}

	// And a successful hydration still matches on the set.
	known := &core.RoutingContext{
		RequestedModel: core.RequestedModel{
			ID: "claude", CandidateIDs: []string{"m-c"},
			CandidateProviderIDs: []string{"anthropic"},
		},
		EndpointType: typology.EndpointKindChat,
	}
	if !RuleMatchesContext(conds, "claude", known) {
		t.Error("a resolved model on the listed provider stopped matching")
	}
}

// TestModelTypes_AudioIsOneValueCoveringThreeEndpoints states the breadth of
// the deprecated `audio` type on the RULE side, where it is widest.
//
// On the catalogue side `audio` is a back-compat fallback: a model row still
// typed coarsely keeps routing on whichever audio endpoint serves it. On the
// rule side the same acceptance means a SINGLE condition fires on all three —
// an admin writing `modelTypes: ["audio"]` for their speech traffic also
// governs text-to-speech and realtime sessions.
//
// Left as it is deliberately. Splitting the value into three would require
// guessing which of the three an existing rule meant, and a guess that lands
// wrong silently re-targets live traffic — the same reason the nested-strategy
// values were not auto-migrated. Recorded here rather than left emergent, so
// the breadth is a stated fact a reader can find; it narrows when the
// deprecation window closes and a migration has retyped the remaining rows.
func TestModelTypes_AudioIsOneValueCoveringThreeEndpoints(t *testing.T) {
	conds := &core.MatchConditions{ModelTypes: []string{"audio"}}

	for _, kind := range []typology.EndpointKind{
		typology.EndpointKindTTS, typology.EndpointKindSTT, typology.EndpointKindRealtime,
	} {
		ctx := &core.RoutingContext{
			RequestedModel: core.RequestedModel{ID: "auto"},
			EndpointType:   kind,
		}
		if !RuleMatchesContext(conds, "auto", ctx) {
			t.Errorf("an audio-typed rule did not match %s — the coarse value is the only thing "+
				"keeping a rule written before the sub-types existed pointed at its traffic", kind)
		}
	}

	// It is broad, not universal. A chat request must not pick it up: that is
	// the defect the coarse type caused on the catalogue side, where gpt-audio-*
	// models are served on chat completions.
	chat := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		EndpointType:   typology.EndpointKindChat,
	}
	if RuleMatchesContext(conds, "auto", chat) {
		t.Error("an audio-typed rule matched a chat request — the value would then govern " +
			"traffic no admin writing `audio` was thinking of")
	}
}
