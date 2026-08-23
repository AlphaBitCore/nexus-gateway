package strategies

import (
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Acceptance is the question "can this candidate take what the request
// carries", and it is the router's own responsibility in a way the floor is
// not. When the CALLER names a model, an upstream refusal is the model's
// limitation honestly reported. When the ROUTER picks the model, a refusal
// caused by that pick is the router's defect — the caller never chose it.
//
// The dimension existed for images only. A request carrying audio, video or a
// document was routed without anyone asking whether the chosen model accepts
// it, so the router handed a text-only model an audio part and produced the
// upstream 400 itself.
//
// Every modality is exercised separately on purpose: a single audio case
// cannot distinguish "the filter iterates what the request carries" from
// "audio was special-cased beside image".

// accepts builds a candidate that declares exactly the given input modalities
// and requires nothing — so these cases isolate acceptance from the floor.
func accepts(code string, inputModalities ...string) core.SmartModelRow {
	return core.SmartModelRow{
		ModelID: code, ModelCode: code, ProviderID: "p1",
		InputModalities: inputModalities,
	}
}

func TestAcceptance_DropsACandidateThatCannotTakeWhatTheRequestCarries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		carries  requestModalities
		modality string
	}{
		{"image", reqImage, "image"},
		{"audio", reqAudio, "audio"},
		{"video", reqVideo, "video"},
		{"file", reqFile, "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped, skipped := filterByCapability(
				[]core.SmartModelRow{
					accepts("text-only", "text"),
					accepts("takes-it", "text", tc.modality),
				},
				false, false, false, reqText|tc.carries, modsOf(reqText|tc.carries))

			if len(kept) != 1 || kept[0].ModelCode != "takes-it" {
				t.Fatalf("kept = %v, want only the model that declares %q — routing to the "+
					"other one produces an upstream 400 the caller never asked for",
					codes(kept), tc.modality)
			}
			if dropped != 1 {
				t.Errorf("dropped = %d, want 1", dropped)
			}
			if len(skipped) != 0 {
				t.Errorf("skipped = %v, want none — the pool did not empty", skipped)
			}
		})
	}
}

// The mirror: a modality the request does NOT carry must not narrow the pool.
// Without this, generalising the filter would quietly make every text-only
// request depend on the catalog's audio/video/file columns.
func TestAcceptance_AModalityTheRequestDoesNotCarryNarrowsNothing(t *testing.T) {
	pool := []core.SmartModelRow{accepts("a", "text"), accepts("b", "text")}
	kept, dropped, skipped := filterByCapability(pool, false, false, false, reqText, modsOf(reqText))
	if len(kept) != 2 || dropped != 0 || len(skipped) != 0 {
		t.Errorf("kept = %v dropped = %d skipped = %v, want an untouched pool",
			codes(kept), dropped, skipped)
	}
}

// Fail-open when the dimension would empty the pool. This is load-bearing for
// documents specifically: NO catalog row declares "file" today, so without it
// every document request loses every candidate and auto-routing stops working
// for the whole class. Asserted rather than assumed.
func TestAcceptance_FileEmptiesThePoolAndIsSkippedRatherThanEnforced(t *testing.T) {
	kept, dropped, skipped := filterByCapability(
		[]core.SmartModelRow{accepts("a", "text"), accepts("b", "text", "image")},
		false, false, false, reqText|reqFile, modsOf(reqText|reqFile))

	if len(kept) != 2 || dropped != 0 {
		t.Fatalf("kept = %v dropped = %d, want both kept — no catalog row declares "+
			"\"file\", so enforcing it would make every document request unroutable",
			codes(kept), dropped)
	}
	if len(skipped) != 1 || skipped[0] != "file" {
		t.Errorf("skipped = %v, want the dimension named so the trace records why", skipped)
	}
}

// An empty InputModalities list is absent catalog metadata, not a declaration
// of "text only". Disqualifying on it would make an unpopulated row unroutable
// for everything but plain text.
func TestAcceptance_AnUndeclaredCandidateIsNotDisqualified(t *testing.T) {
	kept, _, skipped := filterByCapability(
		[]core.SmartModelRow{accepts("undeclared")},
		false, false, false, reqText|reqAudio, modsOf(reqText|reqAudio))
	if len(kept) != 1 {
		t.Errorf("kept = %v, want the undeclared candidate kept (fail-open)", codes(kept))
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none — the candidate passed, the dimension did not empty", skipped)
	}
}

// Several modalities at once narrow cumulatively: only a candidate declaring
// every one of them survives. A per-modality pass that overwrote instead of
// narrowing would keep the last one's winners.
func TestAcceptance_MultipleModalitiesNarrowCumulatively(t *testing.T) {
	kept, _, skipped := filterByCapability(
		[]core.SmartModelRow{
			accepts("image-only", "text", "image"),
			accepts("audio-only", "text", "audio"),
			accepts("both", "text", "image", "audio"),
		},
		false, false, false, reqText|reqImage|reqAudio, modsOf(reqText|reqImage|reqAudio))

	if len(kept) != 1 || kept[0].ModelCode != "both" {
		t.Fatalf("kept = %v, want only the candidate declaring BOTH", codes(kept))
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}
}

// The two vocabularies must agree, and this is where that is asserted rather
// than assumed. The catalog's InputModalities column and normcore's MediaRef
// modality are different fields owned by different layers; the acceptance
// filter compares one against the other, so it only works while they spell the
// same thing. If either side ever moves, this fails here instead of silently
// making every request of that modality unfilterable.
func TestCarriedModalities_MatchTheMediaRefVocabulary(t *testing.T) {
	want := map[requestModalities]string{
		reqImage: normcore.ModalityImage,
		reqAudio: normcore.ModalityAudio,
		reqVideo: normcore.ModalityVideo,
		reqFile:  normcore.ModalityFile,
	}
	if len(carriedModalities) != len(want) {
		t.Fatalf("carriedModalities has %d entries, the MediaRef vocabulary has %d — "+
			"a modality present in one and not the other is one the filter cannot see",
			len(carriedModalities), len(want))
	}
	for _, m := range carriedModalities {
		w, ok := want[m.bit]
		if !ok {
			t.Errorf("carriedModalities carries a bit with no MediaRef modality: %v", m)
			continue
		}
		if m.name != w {
			t.Errorf("catalog name %q != MediaRef modality %q for the same bit; the filter "+
				"compares one against the other and would silently never match", m.name, w)
		}
		// modalityBit is the other direction: the same string must round-trip
		// back to the same bit, or the request's set and the catalog's column
		// are describing different things.
		if got := modalityBit(m.name); got != m.bit {
			t.Errorf("modalityBit(%q) = %v, want %v", m.name, got, m.bit)
		}
	}
}
