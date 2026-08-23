package proxy

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// countingCanonical reports how many times the guard reached for the canonical.
// The whole design rests on that number: materialising is the expensive thing,
// and the reason this guard could be added to the explicit path at all is that
// 198 of 200 catalog models declare no floor, so the snapshot is consulted
// first and the canonical is touched only for the ones that have one.
func countingCanonical(np *normcore.NormalizedPayload, calls *int) canonicalFn {
	return func() *normcore.NormalizedPayload {
		*calls++
		return np
	}
}

func audioRequest() *normcore.NormalizedPayload {
	return &normcore.NormalizedPayload{Messages: []normcore.Message{{
		Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "describe this"},
			{Type: normcore.ContentMedia, MediaRef: &normcore.MediaRef{Modality: "audio"}},
		},
	}}}
}

func textOnlyRequest() *normcore.NormalizedPayload {
	return &normcore.NormalizedPayload{Messages: []normcore.Message{{
		Content: []normcore.ContentBlock{{Type: normcore.ContentText, Text: "hello"}},
	}}}
}

// Built through the production constructor rather than a hand-made map, so the
// fixture cannot drift from how a real snapshot is assembled.
func handlerWithCaps(t *testing.T, required map[string][]string) *Handler {
	t.Helper()
	rows := make([]store.Model, 0, len(required))
	for id, mods := range required {
		rows = append(rows, store.Model{ID: id, RequiredModalities: mods})
	}
	cache := capability.NewCache()
	cache.Replace(capability.NewSnapshot(rows))
	return &Handler{deps: &Deps{CapCache: cache}}
}

// The 198-of-200 case, and the reason this guard is affordable here at all.
func TestFloorGuard_NoFloorDeclared_DoesNotTouchTheCanonical(t *testing.T) {
	h := handlerWithCaps(t, map[string][]string{"m-plain": nil})
	calls := 0
	if err := h.floorGuard("m-plain", "gpt-4o-mini", countingCanonical(textOnlyRequest(), &calls)); err != nil {
		t.Fatalf("a model with no floor must pass: %v", err)
	}
	if calls != 0 {
		t.Errorf("canonical materialised %d time(s) for a model with no floor — that is the cost this design exists to avoid", calls)
	}
}

// The defect: gpt-audio-mini is type=chat and text-accepting, so the modality
// guard passes it for a plain text chat. Only the floor catches that it needs
// audio, and this path never ran the floor.
func TestFloorGuard_FloorUnmet_IsRefusedBeforeDispatch(t *testing.T) {
	h := handlerWithCaps(t, map[string][]string{"m-audio": {"audio"}})
	calls := 0
	err := h.floorGuard("m-audio", "gpt-audio-mini", countingCanonical(textOnlyRequest(), &calls))
	if err == nil {
		t.Fatal("a text-only request to a model requiring audio must not reach the wire")
	}
	fe := &routingFallbackError{}
	ok := errors.As(err, &fe)
	if !ok {
		t.Fatalf("want *routingFallbackError, got %T", err)
	}
	if fe.code != "MODEL_REQUIRED_MODALITY_MISSING" {
		t.Errorf("code=%q", fe.code)
	}
	if !strings.Contains(fe.message, "audio") || !strings.Contains(fe.message, "gpt-audio-mini") {
		t.Errorf("the message must name the missing modality and the model the caller asked for, got %q", fe.message)
	}
	if calls != 1 {
		t.Errorf("canonical materialised %d time(s), want exactly 1", calls)
	}
}

// The other half: a request that DOES carry the required modality passes. A
// guard that refuses everything is not a fix.
func TestFloorGuard_FloorMet_Passes(t *testing.T) {
	h := handlerWithCaps(t, map[string][]string{"m-audio": {"audio"}})
	calls := 0
	if err := h.floorGuard("m-audio", "gpt-audio-mini", countingCanonical(audioRequest(), &calls)); err != nil {
		t.Fatalf("a request carrying the required modality must pass: %v", err)
	}
}

// Text is read off a non-empty text block, not inferred from the presence of
// messages — an image-only turn still has messages and must not satisfy a text
// floor it does not meet.
func TestFloorGuard_EmptyTextDoesNotSatisfyATextFloor(t *testing.T) {
	h := handlerWithCaps(t, map[string][]string{"m-text": {"text"}})
	imageOnly := &normcore.NormalizedPayload{Messages: []normcore.Message{{
		Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: ""},
			{Type: normcore.ContentMedia, MediaRef: &normcore.MediaRef{Modality: "image"}},
		},
	}}}
	calls := 0
	if err := h.floorGuard("m-text", "some-model", countingCanonical(imageOnly, &calls)); err == nil {
		t.Error("an empty text block satisfied a text floor")
	}
}

// Nil-tolerance matches the resolver's: a deployment without a capability
// snapshot skips the check rather than refusing traffic.
func TestFloorGuard_NoSnapshot_SkipsRatherThanRefuses(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	calls := 0
	if err := h.floorGuard("m-audio", "gpt-audio-mini", countingCanonical(textOnlyRequest(), &calls)); err != nil {
		t.Errorf("a missing snapshot must not refuse traffic: %v", err)
	}
	if calls != 0 {
		t.Errorf("canonical materialised %d time(s) with no snapshot to check against", calls)
	}
}

// The embeddings pre-filter, the resolver's other guard this path was missing.

func handlerWithEmbeddingCaps(t *testing.T, modelID string, capJSON string) *Handler {
	t.Helper()
	cache := capability.NewCache()
	cache.Replace(capability.NewSnapshot([]store.Model{{
		ID: modelID, CapabilityJson: []byte(capJSON),
	}}))
	return &Handler{deps: &Deps{CapCache: cache}}
}

func TestEmbeddingCapabilityGuard_UnsupportedDimensionsIsRefused(t *testing.T) {
	h := handlerWithEmbeddingCaps(t, "m-emb", `{"embeddings":{"supported_dimensions":[256,512]}}`)
	body := func() []byte { return []byte(`{"model":"e","input":"hi","dimensions":9999}`) }
	err := h.embeddingCapabilityGuard("m-emb", "text-embedding-x", typology.EndpointKindEmbeddings, body)
	if err == nil {
		t.Fatal("a dimensions value the model does not support must not reach the wire")
	}
	fe := &routingFallbackError{}
	ok := errors.As(err, &fe)
	if !ok {
		t.Fatalf("want *routingFallbackError, got %T", err)
	}
	if fe.code != "MODEL_CAPABILITY_MISMATCH" {
		t.Errorf("code=%q", fe.code)
	}
	if !strings.Contains(fe.message, "text-embedding-x") {
		t.Errorf("the message must name the model the caller asked for, got %q", fe.message)
	}
}

// The other half — a supported request still passes. A guard that refuses
// everything is not a fix.
func TestEmbeddingCapabilityGuard_SupportedRequestPasses(t *testing.T) {
	h := handlerWithEmbeddingCaps(t, "m-emb", `{"embeddings":{"supported_dimensions":[256,512]}}`)
	body := func() []byte { return []byte(`{"model":"e","input":"hi","dimensions":512}`) }
	if err := h.embeddingCapabilityGuard("m-emb", "text-embedding-x", typology.EndpointKindEmbeddings, body); err != nil {
		t.Errorf("a supported request was refused: %v", err)
	}
}

// A non-embeddings endpoint must not pay the body parse at all.
func TestEmbeddingCapabilityGuard_NonEmbeddingsEndpointSkips(t *testing.T) {
	h := handlerWithEmbeddingCaps(t, "m-emb", `{"embeddings":{"supported_dimensions":[256]}}`)
	parsed := 0
	body := func() []byte { parsed++; return []byte(`{"dimensions":9999}`) }
	if err := h.embeddingCapabilityGuard("m-emb", "gpt-4o", typology.EndpointKindChat, body); err != nil {
		t.Errorf("a chat request must not be judged by an embeddings capability: %v", err)
	}
	if parsed != 0 {
		t.Errorf("body parsed %d time(s) for a non-embeddings endpoint", parsed)
	}
}

// No declared embeddings capability means nothing to compare — and the body
// must not be parsed to discover that.
func TestEmbeddingCapabilityGuard_NoDeclaredCapability_SkipsWithoutParsing(t *testing.T) {
	h := handlerWithEmbeddingCaps(t, "m-emb", `{}`)
	parsed := 0
	body := func() []byte { parsed++; return []byte(`{"dimensions":9999}`) }
	if err := h.embeddingCapabilityGuard("m-emb", "text-embedding-x", typology.EndpointKindEmbeddings, body); err != nil {
		t.Errorf("an undeclared capability must not be read as a constraint: %v", err)
	}
	if parsed != 0 {
		t.Errorf("body parsed %d time(s) with no capability to compare against", parsed)
	}
}

// Nil-tolerance on the embeddings side, matching the resolver's: a deployment
// with no capability snapshot skips rather than refusing traffic.
func TestEmbeddingCapabilityGuard_NoSnapshot_SkipsRatherThanRefuses(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	parsed := 0
	body := func() []byte { parsed++; return []byte(`{"dimensions":9999}`) }
	if err := h.embeddingCapabilityGuard("m", "e", typology.EndpointKindEmbeddings, body); err != nil {
		t.Errorf("a missing snapshot must not refuse traffic: %v", err)
	}
	if parsed != 0 {
		t.Errorf("body parsed %d time(s) with no snapshot", parsed)
	}
}

// A nil body accessor is the shape a caller that has nothing to offer passes.
// It must skip, not panic.
func TestEmbeddingCapabilityGuard_NilBodyAccessor_Skips(t *testing.T) {
	h := handlerWithEmbeddingCaps(t, "m-emb", `{"embeddings":{"supported_dimensions":[256]}}`)
	if err := h.embeddingCapabilityGuard("m-emb", "e", typology.EndpointKindEmbeddings, nil); err != nil {
		t.Errorf("a nil body accessor must skip: %v", err)
	}
}

// An empty body yields no parsed params; there is nothing to judge, so the
// request proceeds rather than being refused for a parameter it never sent.
func TestEmbeddingCapabilityGuard_EmptyBody_Proceeds(t *testing.T) {
	h := handlerWithEmbeddingCaps(t, "m-emb", `{"embeddings":{"supported_dimensions":[256]}}`)
	if err := h.embeddingCapabilityGuard("m-emb", "e", typology.EndpointKindEmbeddings,
		func() []byte { return nil }); err != nil {
		t.Errorf("an empty body must not be refused for a parameter it never carried: %v", err)
	}
}

// A model the snapshot has never seen has no capability to compare against.
func TestFloorGuard_UnknownModel_Passes(t *testing.T) {
	h := handlerWithCaps(t, map[string][]string{"m-known": {"audio"}})
	calls := 0
	if err := h.floorGuard("m-unknown", "whatever", countingCanonical(textOnlyRequest(), &calls)); err != nil {
		t.Errorf("an unknown model must not be refused by a floor nobody declared: %v", err)
	}
	if calls != 0 {
		t.Errorf("canonical materialised %d time(s) for a model absent from the snapshot", calls)
	}
}

// A nil canonical accessor skips rather than panicking.
func TestFloorGuard_NilCanonicalAccessor_Skips(t *testing.T) {
	h := handlerWithCaps(t, map[string][]string{"m-audio": {"audio"}})
	if err := h.floorGuard("m-audio", "gpt-audio-mini", nil); err != nil {
		t.Errorf("a nil canonical accessor must skip: %v", err)
	}
}

// The point of this guard is that the two selection paths agree, so the
// verdict must be capability.Compatible's — not a second opinion layered on
// top of it. A block that declares no supported_dimensions refuses an explicit
// dimensions request (fail-safe: an undeclared list is not a promise), and the
// explicit path must refuse it for the same reason the resolver does.
//
// Asserted against Compatible directly rather than against a remembered
// expectation: a hand-written "want" here would be a third copy of the rule,
// free to drift from the one that actually decides.
func TestEmbeddingCapabilityGuard_VerdictMatchesTheResolvers(t *testing.T) {
	h := handlerWithEmbeddingCaps(t, "m-emb", `{"embeddings":{"max_batch_size":100}}`)
	raw := []byte(`{"model":"e","input":"hi","dimensions":4096}`)

	params := parseEmbeddingRequest(raw)
	wantOK, _, _ := capability.Compatible(&capability.EmbeddingRequest{
		Dimensions:     params.Dimensions,
		BatchSize:      params.BatchSize,
		EncodingFormat: params.EncodingFormat,
		InputType:      params.InputType,
		TaskType:       params.TaskType,
	}, h.deps.CapCache.Load().Get("m-emb"))

	err := h.embeddingCapabilityGuard("m-emb", "e", typology.EndpointKindEmbeddings,
		func() []byte { return raw })
	if gotOK := err == nil; gotOK != wantOK {
		t.Errorf("explicit path says ok=%v, capability.Compatible says ok=%v — the two selection paths disagree, "+
			"which is the divergence this guard exists to remove", gotOK, wantOK)
	}
}

// TestNamedModelModalityGuard_FlagGovernsTheNamedVerdict is the #297 red-proof
// for the passthrough gate. The floor and the input-modality ceiling are the
// gateway's OWN modality verdict on a model the caller NAMED; by default that
// verdict is the upstream's (our catalogue can be wrong), and only
// EnforceNamedModelModality makes the gateway enforce it locally.
//
// The two flag-OFF cases are the ones that fail if the gate is removed — a
// guard run unconditionally would 400 a named request the default is meant to
// forward. The two flag-ON cases pin that the guards still fire (and with the
// right codes) when an operator opts in. The embeddings guard is verified
// separately and deliberately outside this method — it is not modality.
func TestNamedModelModalityGuard_FlagGovernsTheNamedVerdict(t *testing.T) {
	// "image" must be a DESCRIBED input modality for the ceiling to enforce it,
	// so m-vision declares it; m-text does not (the ceiling target); m-audio
	// REQUIRES audio (the floor target).
	rows := []store.Model{
		{ID: "m-text", InputModalities: []string{"text"}},
		{ID: "m-vision", InputModalities: []string{"text", "image"}},
		{ID: "m-audio", InputModalities: []string{"text", "audio"}, RequiredModalities: []string{"audio"}},
	}
	build := func(enforce bool) *Handler {
		cache := capability.NewCache()
		cache.Replace(capability.NewSnapshot(rows))
		return &Handler{deps: &Deps{CapCache: cache, EnforceNamedModelModality: enforce}}
	}
	imageReq := func() *normcore.NormalizedPayload {
		return &normcore.NormalizedPayload{Messages: []normcore.Message{{
			Content: []normcore.ContentBlock{
				{Type: normcore.ContentText, Text: "look"},
				{Type: normcore.ContentMedia, MediaRef: &normcore.MediaRef{Modality: "image"}},
			},
		}}}
	}
	wantCode := func(t *testing.T, err error, code string) {
		t.Helper()
		fe := &routingFallbackError{}
		if err == nil || !errors.As(err, &fe) || fe.code != code {
			t.Errorf("want %s, got %v", code, err)
		}
	}

	// FLOOR — an audio-requiring model, a text-only request.
	if err := build(false).namedModelModalityGuard("m-audio", "gpt-audio-mini",
		textOnlyRequest); err != nil {
		t.Errorf("flag OFF: a NAMED audio-requiring model got OUR floor 400 (%v) — the default "+
			"defers a named model's modality verdict to the upstream", err)
	}
	wantCode(t, build(true).namedModelModalityGuard("m-audio", "gpt-audio-mini",
		textOnlyRequest), "MODEL_REQUIRED_MODALITY_MISSING")

	// CEILING — a text-only model, an image-carrying request.
	if err := build(false).namedModelModalityGuard("m-text", "text-only-model", imageReq); err != nil {
		t.Errorf("flag OFF: a NAMED text-only model got OUR ceiling 400 (%v) — the default defers "+
			"a named model's modality verdict to the upstream", err)
	}
	wantCode(t, build(true).namedModelModalityGuard("m-text", "text-only-model", imageReq),
		"MODEL_INPUT_MODALITY_UNSUPPORTED")
}

// TestFloorGuard_AgreesWithTheRoutingPredicate pins the two selection paths to
// one answer, and pins that answer to an independently stated expectation.
//
// A model's floor is checked twice: here, for a caller who named the model, and
// inside routing, for a model the gateway chose. Comparing the two to each other
// was enough while they had separate implementations — it is how the `text`
// disagreement was found. Now that both call the same helper, agreement alone
// proves nothing: break the helper and the two agree on the wrong answer while
// the test stays green. Verified by mutation, so the third column is not
// decoration.
//
// So each case says what the verdict SHOULD be. The comparison then catches a
// guard drifting away from the helper, and the expectation catches the helper
// itself regressing.
func TestFloorGuard_AgreesWithTheRoutingPredicate(t *testing.T) {
	imageOnly := &normcore.NormalizedPayload{Messages: []normcore.Message{{
		Content: []normcore.ContentBlock{
			{Type: normcore.ContentMedia, MediaRef: &normcore.MediaRef{Modality: "image"}},
		},
	}}}

	cases := []struct {
		name         string
		required     []string
		payload      *normcore.NormalizedPayload
		wantAdmitted bool
	}{
		{"text floor, request carries no text", []string{"text"}, imageOnly, false},
		{"text floor, request carries text", []string{"text"}, textOnlyRequest(), true},
		{"audio floor, request carries audio", []string{"audio"}, audioRequest(), true},
		{"audio floor, request carries none", []string{"audio"}, textOnlyRequest(), false},
		{"mixed floor, request carries one", []string{"text", "audio"}, imageOnly, false},
		{"no floor declared is not a floor to miss", nil, imageOnly, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := handlerWithCaps(t, map[string][]string{"m": tc.required})
			calls := 0

			guardAdmits := h.floorGuard("m", "some-model", countingCanonical(tc.payload, &calls)) == nil
			routingAdmits := capability.DeclarationMeetsFloor(tc.required, capability.CarriedModalities(tc.payload))

			if guardAdmits != routingAdmits {
				t.Errorf("the two selection paths disagree: naming the model %v, "+
					"letting the gateway pick it %v", guardAdmits, routingAdmits)
			}
			if guardAdmits != tc.wantAdmitted {
				t.Errorf("admitted=%v, want %v — both paths agree on the wrong answer",
					guardAdmits, tc.wantAdmitted)
			}
		})
	}
}
