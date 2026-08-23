package capability

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// DescribedInputModalities is what lets a guard tell "this model says no" apart
// from "the catalog has no opinion". It must report the UNION across the
// snapshot, lower-cased, so a modality declared by one model is enforceable
// against every other.
func TestDescribedInputModalities_UnionsAcrossTheSnapshot(t *testing.T) {
	snap := NewSnapshot([]store.Model{
		{ID: "a", InputModalities: []string{"text"}},
		{ID: "b", InputModalities: []string{"Text", "IMAGE"}},
		{ID: "c", InputModalities: []string{"audio"}},
	})
	got := snap.DescribedInputModalities()
	for _, want := range []string{"text", "image", "audio"} {
		if !got[want] {
			t.Errorf("%q declared by some model but not reported as described: %v", want, got)
		}
	}
	if got["file"] {
		t.Error("file is declared by no model here and must not be reported as described — that is what stops a guard refusing every document request")
	}
	if len(got) != 3 {
		t.Errorf("unexpected extra entries: %v", got)
	}
}

// A nil snapshot answers "nothing is described", so a guard consulting it
// refuses nothing rather than panicking or refusing everything.
func TestDescribedInputModalities_NilSnapshotDescribesNothing(t *testing.T) {
	var snap *Snapshot
	if got := snap.DescribedInputModalities(); len(got) != 0 {
		t.Errorf("nil snapshot must describe nothing, got %v", got)
	}
}

// A model whose capability row carries no modalities contributes nothing rather
// than an empty-string entry, which would make "" a described modality and
// change what a guard enforces.
func TestDescribedInputModalities_ModelsWithNoModalitiesContributeNothing(t *testing.T) {
	snap := NewSnapshot([]store.Model{
		{ID: "a", InputModalities: nil},
		{ID: "b", InputModalities: []string{}},
		{ID: "c", InputModalities: []string{"text"}},
	})
	got := snap.DescribedInputModalities()
	if got[""] {
		t.Error(`the empty string must never become a "described" modality`)
	}
	if len(got) != 1 || !got["text"] {
		t.Errorf("want only text, got %v", got)
	}
}

// TestDescribes_SeparatesAModelSayingNoFromACatalogWithNoOpinion.
//
// The two look identical to a caller holding one model: neither declares
// "video". They mean opposite things. If other rows declare video, this row's
// omission is a refusal and the target must go. If NO row declares it — true of
// file and video across every chat model — the silence is a fact about how the
// catalogue is populated, and refusing on it rejects every document request,
// including all the ones that would have worked.
//
// This is the escape hatch for callers whose own list is too small to tell:
// the smart strategy's pool IS the catalogue, so an emptied dimension there
// says the same thing, but a recovery list of two admin-named models says
// nothing at all.
func TestDescribes_SeparatesAModelSayingNoFromACatalogWithNoOpinion(t *testing.T) {
	snap := NewSnapshot([]store.Model{
		{ID: "vision", InputModalities: []string{"text", "image"}},
		{ID: "text-only", InputModalities: []string{"text"}},
	})

	if !snap.Describes("image") {
		t.Error("image is declared by one model, so text-only's omission is a refusal that " +
			"must be enforceable — reading it as 'no opinion' lets a text-only model take " +
			"an image request")
	}
	if snap.Describes("video") {
		t.Error("no row declares video; treating the catalogue's silence as a refusal " +
			"rejects every video request, including the ones that work")
	}
	if !snap.Describes("IMAGE") {
		t.Error("Describes must fold case — a declaration written in caps would otherwise " +
			"leave that dimension unenforceable across the whole snapshot")
	}

	var nilSnap *Snapshot
	if nilSnap.Describes("image") {
		t.Error("a nil snapshot must describe nothing, so a guard consulting it refuses nothing")
	}
}

// TestAcceptsModality_IsThePerDimensionCeiling.
//
// Callers that hold a candidate list apply the ceiling one modality at a time,
// because the escape hatch is per dimension: a request carrying image AND file
// can be refused on image (some model declares it) while file goes unjudged
// (none does). Answering for the whole request at once collapses those two into
// one verdict and takes the wrong branch for one of them.
func TestAcceptsModality_IsThePerDimensionCeiling(t *testing.T) {
	snap := NewSnapshot([]store.Model{
		{ID: "vision", InputModalities: []string{"text", "image"}},
		{ID: "text-only", InputModalities: []string{"text"}},
		{ID: "undeclared"},
	})

	if snap.AcceptsModality("text-only", "image") {
		t.Error("a model declaring only text accepted an image")
	}
	if !snap.AcceptsModality("vision", "image") {
		t.Error("a model declaring image was refused one")
	}
	if !snap.AcceptsModality("text-only", "text") {
		t.Error("text is never a restriction — refusing it here would drop every model " +
			"whose declaration omits the obvious")
	}
	if !snap.AcceptsModality("undeclared", "image") {
		t.Error("an empty declaration is UNDECLARED, not text-only; reading it as a " +
			"restriction hid every model that never filled the field in")
	}
	if !snap.AcceptsModality("not-in-snapshot", "image") {
		t.Error("a model the snapshot has never seen must not be refused — the snapshot " +
			"reloads independently of routing and would otherwise drop live targets")
	}

	var nilSnap *Snapshot
	if !nilSnap.AcceptsModality("anything", "image") {
		t.Error("a nil snapshot must refuse nothing; capability filtering is off, not inverted")
	}
}

// TestMeetsFloor_AbsentDataRefusesNothing.
//
// The floor drops a model that REQUIRES a modality the request does not carry —
// gpt-audio-mini on a plain-text request. It must not fire when the data to
// judge is missing: a nil snapshot means filtering is off, and a model the
// snapshot has not seen is unjudged, not unqualified. Getting this backwards
// turns a snapshot reload into a window where every request is refused.
func TestMeetsFloor_AbsentDataRefusesNothing(t *testing.T) {
	snap := NewSnapshot([]store.Model{
		{ID: "audio-in", RequiredModalities: []string{"audio"}},
	})

	if snap.MeetsFloor("audio-in", []string{"text"}) {
		t.Error("a model requiring audio was kept for a text-only request; it fails at the " +
			"provider with a wire error about a missing field")
	}
	if !snap.MeetsFloor("audio-in", []string{"text", "audio"}) {
		t.Error("a request carrying audio was refused by the model that requires it")
	}
	if !snap.MeetsFloor("never-seen", []string{"text"}) {
		t.Error("a model absent from the snapshot was refused — during a reload that is " +
			"every model")
	}

	var nilSnap *Snapshot
	if !nilSnap.MeetsFloor("audio-in", []string{"text"}) {
		t.Error("a nil snapshot must refuse nothing")
	}
}
