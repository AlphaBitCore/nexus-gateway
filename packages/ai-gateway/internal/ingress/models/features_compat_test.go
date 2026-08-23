package models

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// The contract this exists to preserve: an SDK caller that has read
// features ∋ "vision" since before the modality arrays existed keeps reading
// it, from a row that no longer stores the string.
func TestWithDerivedFeatures_ChatWithImageInputStillAdvertisesVision(t *testing.T) {
	got := withDerivedFeatures("chat", []string{"function_calling"}, []string{"text", "image"})
	if !slices.Equal(got, []string{"function_calling", "vision"}) {
		t.Errorf("features = %v, want [function_calling vision]", got)
	}
}

func TestWithDerivedFeatures_TextOnlyChatDoesNotClaimVision(t *testing.T) {
	got := withDerivedFeatures("chat", []string{"streaming"}, []string{"text"})
	if slices.Contains(got, "vision") {
		t.Errorf("features = %v, a text-only model must not advertise vision", got)
	}
}

// gemini-embedding-2 is the one catalog row where this is observable: it
// accepts images and is not a chat model. Calling it a vision model would be a
// new claim invented by the derivation, not a contract being preserved.
func TestWithDerivedFeatures_ImageAcceptingEmbeddingIsNotAVisionModel(t *testing.T) {
	got := withDerivedFeatures("embedding", []string{}, []string{"text", "image"})
	if slices.Contains(got, "vision") {
		t.Errorf("features = %v, an embeddings model is not a vision model", got)
	}
}

// A row written before the fold — or restored from an old backup — may still
// carry the string. Serving it twice would be a visible defect on a list
// endpoint SDKs parse.
func TestWithDerivedFeatures_DoesNotDuplicateAStoredVision(t *testing.T) {
	got := withDerivedFeatures("chat", []string{"vision"}, []string{"text", "image"})
	if !slices.Equal(got, []string{"vision"}) {
		t.Errorf("features = %v, want a single vision entry", got)
	}
}

// The store row's slice is shared by every entry built from the same cached
// catalog. Appending in place would let one request's response mutate the
// cache and hand the next request a feature list that grows on every call.
func TestWithDerivedFeatures_DoesNotMutateTheStoredSlice(t *testing.T) {
	stored := make([]string, 1, 8) // spare capacity: append would write in place
	stored[0] = "streaming"
	got := withDerivedFeatures("chat", stored, []string{"text", "image"})
	if len(stored) != 1 || stored[0] != "streaming" {
		t.Errorf("stored features were mutated to %v", stored)
	}
	if !slices.Equal(got, []string{"streaming", "vision"}) {
		t.Errorf("features = %v, want [streaming vision]", got)
	}
}

// The floor has to reach the caller. It was added to the store, the capability
// snapshot and the router, and omitted from the one surface an SDK can read —
// so a client could not tell that gpt-audio-mini is type=chat and still
// refuses a text-only request. The smoke suite hit the same wall: it built an
// index of model floors from this endpoint and the index came back empty.
func TestModelEntries_CarryTheFloorInBothShapes(t *testing.T) {
	m := store.Model{
		Code: "gpt-audio-mini", Type: "chat",
		InputModalities:    []string{"text", "audio"},
		RequiredModalities: []string{"audio"},
	}
	oa := buildOpenAIModelEntry(m, 0)
	if !slices.Equal(oa.RequiredModalities, []string{"audio"}) {
		t.Errorf("OpenAI entry requiredModalities = %v, want [audio]", oa.RequiredModalities)
	}
	an := buildAnthropicModelEntry(m, "")
	if !slices.Equal(an.RequiredModalities, []string{"audio"}) {
		t.Errorf("Anthropic entry requiredModalities = %v, want [audio]", an.RequiredModalities)
	}
}

// Empty is the normal case — 198 of 200 models — and must serialise away
// rather than appear as an empty array on every entry.
func TestModelEntries_OmitAnEmptyFloor(t *testing.T) {
	oa := buildOpenAIModelEntry(store.Model{Code: "gpt-4o", Type: "chat"}, 0)
	b, err := json.Marshal(oa)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "requiredModalities") {
		t.Errorf("entry = %s; an unconstrained model must not carry the key", b)
	}
}
