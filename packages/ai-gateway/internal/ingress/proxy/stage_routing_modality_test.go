package proxy

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// modalitySnapshot builds a capability snapshot from (modelID → inputModalities)
// so each test states the catalog it is reasoning about, rather than depending
// on whatever the real catalog happens to hold.
func modalitySnapshot(t *testing.T, byModel map[string][]string) *capability.Snapshot {
	t.Helper()
	var models []store.Model
	for id, mods := range byModel {
		models = append(models, store.Model{ID: id, InputModalities: mods})
	}
	return capability.NewSnapshot(models)
}

func modalityHandler(t *testing.T, snap *capability.Snapshot) *Handler {
	t.Helper()
	c := capability.NewCache()
	c.Replace(snap)
	return &Handler{deps: &Deps{CapCache: c}}
}

func payloadWith(modalities ...string) canonicalFn {
	var blocks []normcore.ContentBlock
	for _, m := range modalities {
		if m == "text" {
			blocks = append(blocks, normcore.ContentBlock{Type: normcore.ContentText, Text: "hi"})
			continue
		}
		blocks = append(blocks, normcore.ContentBlock{
			Type:     normcore.ContentMedia,
			MediaRef: &normcore.MediaRef{Modality: m},
		})
	}
	np := &normcore.NormalizedPayload{Messages: []normcore.Message{{Content: blocks}}}
	return func() *normcore.NormalizedPayload { return np }
}

// An image sent to a text-only model is refused here, naming the modality,
// rather than forwarded to earn the provider's own 400.
func TestInputModalityGuard_RefusesAModalityTheModelDoesNotAccept(t *testing.T) {
	h := modalityHandler(t, modalitySnapshot(t, map[string][]string{
		"text-only": {"text"},
		"visionary": {"text", "image"},
	}))
	err := h.inputModalityGuard("text-only", "gpt-textonly", payloadWith("text", "image"))
	if err == nil {
		t.Fatal("an image sent to a text-only model must be refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "image") {
		t.Errorf("the error must name the offending modality, got %q", msg)
	}
	if strings.Contains(msg, "text,") || strings.Contains(msg, "text ") && strings.Contains(msg, "does not accept text") {
		t.Errorf("text is accepted and must not be reported as rejected: %q", msg)
	}
}

// A guard proven to reject is not thereby proven to accept. This is the case
// that catches a guard whose predicate is inverted or whose set is empty.
func TestInputModalityGuard_AcceptsAModalityTheModelDeclares(t *testing.T) {
	h := modalityHandler(t, modalitySnapshot(t, map[string][]string{
		"visionary": {"text", "image"},
	}))
	if err := h.inputModalityGuard("visionary", "gpt-vision", payloadWith("text", "image")); err != nil {
		t.Fatalf("a model that declares image must accept one: %v", err)
	}
}

// The load-bearing case. No catalog entry declares "file", so its absence from
// a given model says nothing about that model. Refusing on it would reject
// every document request, including the Anthropic / Gemini / Responses lanes
// that carry one correctly. The guard must stay silent until the catalog has an
// opinion — and start enforcing with no code change once it does.
func TestInputModalityGuard_SilentOnAModalityNoModelDescribes(t *testing.T) {
	h := modalityHandler(t, modalitySnapshot(t, map[string][]string{
		"text-only": {"text"},
		"visionary": {"text", "image"},
	}))
	if err := h.inputModalityGuard("text-only", "gpt-textonly", payloadWith("text", "file")); err != nil {
		t.Fatalf("no model declares file, so file must not be refused anywhere: %v", err)
	}

	// The same request, once ONE model in the catalog declares file: the
	// vocabulary is now described, so a model omitting it is genuinely saying no.
	h2 := modalityHandler(t, modalitySnapshot(t, map[string][]string{
		"text-only": {"text"},
		"docreader": {"text", "file"},
	}))
	err := h2.inputModalityGuard("text-only", "gpt-textonly", payloadWith("text", "file"))
	if err == nil {
		t.Fatal("once the catalog describes file, a model that omits it must refuse — otherwise the guard can never enforce it")
	}
	if !strings.Contains(err.Error(), "file") {
		t.Errorf("error must name file, got %q", err)
	}
}

// A model with no declared capability is not a model that accepts nothing.
func TestInputModalityGuard_NoDeclaredCapabilityIsNotAConstraint(t *testing.T) {
	h := modalityHandler(t, modalitySnapshot(t, map[string][]string{"visionary": {"text", "image"}}))
	if err := h.inputModalityGuard("unknown-model", "mystery", payloadWith("text", "image")); err != nil {
		t.Fatalf("an unknown model must not be refused on capability data we do not have: %v", err)
	}
}

// Text is never refused, even by a model whose declaration omits it.
//
// Four shipped models omit "text" from inputModalities — whisper-1 and the
// gpt-4o-transcribe family — because their declaration describes the audio they
// transcribe. But a transcription request legitimately carries text alongside
// the audio: the prompt that biases the decoding. Refusing it would reject a
// request the provider accepts.
//
// The ceiling predicate encodes this by admitting text unconditionally, and
// this guard now asks that predicate instead of its own map. The two disagreed
// before: the guard refused what routing admitted, on the same model and the
// same request.
func TestInputModalityGuard_TextIsNeverRefused(t *testing.T) {
	// Exactly the shipped shape: audio declared, text absent.
	h := modalityHandler(t, modalitySnapshot(t, map[string][]string{
		"whisper":   {"audio"},
		"some-chat": {"text", "image"}, // makes "text" a described modality
	}))

	if err := h.inputModalityGuard("whisper", "whisper-1", payloadWith("text", "audio")); err != nil {
		t.Fatalf("a transcription prompt must reach the model that transcribes: %v", err)
	}
	if err := h.inputModalityGuard("whisper", "whisper-1", payloadWith("text")); err != nil {
		t.Errorf("text alone must not be refused by a model that omits it: %v", err)
	}

	// The permissiveness is scoped to text: a modality the model genuinely does
	// not declare is still refused, so this is not a guard that passes anything.
	if err := h.inputModalityGuard("whisper", "whisper-1", payloadWith("text", "image")); err == nil {
		t.Error("image reached a model declaring only audio")
	}
}
