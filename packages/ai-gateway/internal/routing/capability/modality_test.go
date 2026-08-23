package capability_test

import (
	"reflect"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
)

// Built through the constructor the production path uses, from store.Model rows,
// so the test cannot pass on a shape the loader never produces.
func snap(t *testing.T, caps map[string]capability.ModelCapability) *capability.Snapshot {
	t.Helper()
	var models []store.Model
	for id, c := range caps {
		models = append(models, store.Model{
			ID:                 id,
			InputModalities:    c.InputModalities,
			RequiredModalities: c.RequiredModalities,
		})
	}
	return capability.NewSnapshot(models)
}

// The predicate every stage must consult. Its two fail-open directions are not
// leniency — each one has a production failure behind it.
func TestAccepts(t *testing.T) {
	s := snap(t, map[string]capability.ModelCapability{
		"vision":     {InputModalities: []string{"text", "image"}},
		"text-only":  {InputModalities: []string{"text"}},
		"undeclared": {},
		// Declares image and NOT text. The carve-out exists for exactly this
		// row shape: a catalog that lists only the interesting modality would
		// otherwise have every one of its models refuse every request, since
		// every chat request carries text.
		"image-no-text": {InputModalities: []string{"image"}},
		"audio-only":    {InputModalities: []string{"text", "audio"}, RequiredModalities: []string{"audio"}},
	})
	for _, tc := range []struct {
		name    string
		model   string
		carried []string
		want    bool
		why     string
	}{
		{"declared image, request carries image", "vision", []string{"image"}, true, ""},
		{"text-only model, request carries audio", "text-only", []string{"audio"}, false,
			"this is the moonshot case: it declares no audio and must not serve one"},
		{"text-only model, plain text request", "text-only", []string{}, true, ""},
		{"undeclared model passes", "undeclared", []string{"image", "file"}, true,
			"an empty list means undeclared, not text-only — reading it as a restriction " +
				"hid 36 document-capable models from routing"},
		{"unknown model passes", "never-heard-of-it", []string{"audio"}, true,
			"a snapshot that has not loaded must not refuse every request"},
		{"floor unmet: audio model, text request", "audio-only", []string{}, false,
			"gpt-audio requires audio and 400s without it"},
		{"floor met", "audio-only", []string{"audio"}, true, ""},
		{"text never discriminates", "image-no-text", []string{"text", "image"}, true,
			"every chat wire carries text; treating it as a dimension would refuse " +
				"every model that lists image but not text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Accepts(tc.model, tc.carried); got != tc.want {
				t.Errorf("Accepts(%q, %v) = %v, want %v — %s",
					tc.model, tc.carried, got, tc.want, tc.why)
			}
		})
	}
}

// A nil snapshot is "no opinion", never "refuse everything".
func TestAccepts_NilSnapshotIsPermissive(t *testing.T) {
	var s *capability.Snapshot
	if !s.Accepts("anything", []string{"audio"}) {
		t.Error("a nil snapshot refused a request; no snapshot means no opinion")
	}
}

// TestMissingFloor is the home-package gate for the floor.
//
// The floor decides whether a model that REQUIRES something can serve a request
// that may not carry it — gpt-audio-mini needs audio, and a text-only request
// will 400 upstream no matter how well-formed it is. Until now the only test
// asserting these semantics lived in the proxy package, written for a different
// property; a mutation removing the rule entirely from this package went
// unnoticed here, in the strategies, and in the resolver.
//
// Text carries the weight of the recent change. It used to be skipped
// unconditionally, because the carried set was built from media blocks only and
// could never report it. Now the builder reports it and no requirement is
// special.
func TestMissingFloor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required []string
		carried  []string
		want     []string
	}{
		{"no requirement is nothing to miss", nil, []string{"text"}, nil},
		{"no requirement, nothing carried", nil, nil, nil},

		{"audio required and carried", []string{"audio"}, []string{"text", "audio"}, nil},
		{"audio required, text-only request", []string{"audio"}, []string{"text"}, []string{"audio"}},
		{"audio required, nothing carried", []string{"audio"}, nil, []string{"audio"}},

		// The semantics that changed. A model declaring a text floor is checked
		// like any other; previously it was admitted whatever the request held.
		{"text required and carried", []string{"text"}, []string{"text"}, nil},
		{"text required, image-only request", []string{"text"}, []string{"image"}, []string{"text"}},

		{"several required, one missing", []string{"text", "audio"}, []string{"text"}, []string{"audio"}},
		{"several required, all missing, all named",
			[]string{"text", "audio"}, []string{"image"}, []string{"text", "audio"}},

		// A requirement nobody recognises is unsatisfiable, not absent: the
		// permissive reading would dispatch to a model whose stated need we
		// could not even evaluate.
		{"an unrecognised requirement is missing", []string{"hologram"}, []string{"text"}, []string{"hologram"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capability.MissingFloor(tc.required, tc.carried)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MissingFloor(%v, %v) = %v, want %v", tc.required, tc.carried, got, tc.want)
			}
			// The boolean form must never disagree with the list it wraps —
			// two answers to one question is the defect this package exists to
			// have exactly one of.
			if met := capability.DeclarationMeetsFloor(tc.required, tc.carried); met != (len(tc.want) == 0) {
				t.Errorf("DeclarationMeetsFloor = %v but MissingFloor reported %v", met, got)
			}
		})
	}
}
