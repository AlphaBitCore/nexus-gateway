package strategies

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func row(code string, required ...string) core.SmartModelRow {
	return core.SmartModelRow{
		ModelID: code, ModelCode: code, ProviderID: "p1",
		InputModalities: []string{"text", "audio"}, RequiredModalities: required,
	}
}

// The defect this dimension exists for: gpt-audio-mini is type=chat and
// accepts text, so it lands in the candidate pool for every plain text
// request, gets selected, and 400s upstream. Sixteen such 400s were measured.
func TestFilterByCapability_DropsAModelThatNeedsWhatTheRequestLacks(t *testing.T) {
	kept, dropped, skipped := filterByCapability(
		[]core.SmartModelRow{row("gpt-4o"), row("gpt-audio-mini", "audio")},
		false, false, false, reqText, modsOf(reqText))
	if len(kept) != 1 || kept[0].ModelCode != "gpt-4o" {
		t.Fatalf("kept = %v, want only gpt-4o", codes(kept))
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none — the pool did not empty", skipped)
	}
}

// The same model must survive when the request actually carries audio;
// otherwise the filter has made it unroutable rather than correctly routed.
func TestFilterByCapability_KeepsItWhenTheRequestCarriesTheModality(t *testing.T) {
	kept, dropped, _ := filterByCapability(
		[]core.SmartModelRow{row("gpt-audio-mini", "audio")},
		false, false, false, reqText|reqAudio, modsOf(reqText|reqAudio))
	if len(kept) != 1 || dropped != 0 {
		t.Errorf("kept = %v dropped = %d, want the model kept", codes(kept), dropped)
	}
}

// The floor does NOT fail open, and that is the difference from the dimensions
// above it.
//
// A ceiling emptying the pool is evidence about the CATALOGUE — no row declares
// `file`, so enforcing that dimension would refuse every document request while
// the wires read them fine. A floor is never that. It says this candidate needs
// something the request does not carry, which is a fact about the candidate and
// stays true however many others share it: a pool of nothing but
// audio-requiring models cannot serve a text request, and answering "none can"
// is true where keeping them routes the request to a model that replies 400.
//
// This test previously asserted the opposite, on the same fail-open reasoning
// the acceptance dimensions use. It was not stale about fail-open being right
// THERE; it was applying it to the one dimension whose emptiness is not about
// the catalogue. The recovery-side filter has always read the floor this way,
// so the old behaviour also made this the U-series defect: one question, two
// answers, in two places.
func TestFilterByCapability_TheFloorDoesNotRelaxWhenItEmptiesThePool(t *testing.T) {
	kept, dropped, skipped := filterByCapability(
		[]core.SmartModelRow{row("a", "audio"), row("b", "audio")},
		false, false, false, reqText, modsOf(reqText))
	if len(kept) != 0 || dropped != 2 {
		t.Errorf("kept = %v dropped = %d, want both dropped — a model that requires audio "+
			"cannot serve a text request, and keeping it sends the request to a 400",
			codes(kept), dropped)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none — the floor is not a dimension that can be waived, "+
			"and naming it in the trace claims the pool was spared when it was not", skipped)
	}
}

// A floor we cannot evaluate must not silently clear. If a future catalog
// writes a modality this build does not know, the safe reading is "this model
// needs something I cannot confirm the request has".
func TestFilterByCapability_AnUnknownRequirementIsNeverSatisfied(t *testing.T) {
	// The carried set matches what row() declares it accepts, so the acceptance
	// dimensions pass cleanly and this case isolates the floor. Carrying
	// modalities no candidate declares would make acceptance fail open and skip,
	// which is correct behaviour but noise for the question asked here.
	kept, _, skipped := filterByCapability(
		[]core.SmartModelRow{row("known"), row("weird", "hologram")},
		false, false, false, reqText|reqAudio, modsOf(reqText|reqAudio))
	if len(kept) != 1 || kept[0].ModelCode != "known" {
		t.Errorf("kept = %v, want the unknown requirement to disqualify", codes(kept))
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}
}

// Empty is the normal case — 198 of 200 catalog models — and must cost
// nothing behaviourally.
func TestFilterByCapability_NoFloorMeansNoConstraint(t *testing.T) {
	kept, dropped, skipped := filterByCapability(
		[]core.SmartModelRow{row("a"), row("b")}, false, false, false, 0, nil)
	if len(kept) != 2 || dropped != 0 || len(skipped) != 0 {
		t.Errorf("kept = %v dropped = %d skipped = %v, want an untouched pool", codes(kept), dropped, skipped)
	}
}

func codes(rows []core.SmartModelRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ModelCode
	}
	return out
}
