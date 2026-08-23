package strategies

// One question, two implementations, and a gate that fails the day they answer
// differently.
//
// "Can this model serve a request carrying these modalities" is asked in two
// places: filterByCapability here, over SmartModelRow with a modality BITMASK,
// and capability.Snapshot.Accepts in the resolver, over ModelCapability with a
// string slice. They read different structs from different sources on different
// stages of the pipeline, and a recovery target that one admitted and the other
// would have dropped is exactly the defect this program shipped.
//
// The obvious fix — delete one and call the other — was written, measured, and
// rejected on the number. filterByCapabilityViaAccepts at the bottom of this
// file IS that collapse, kept as a benchmark arm; over the shipped catalog
// (BenchmarkCollapse_*, count=10):
//
//	text-only   7.8µs →  27.8µs   32Ki → 111Ki    1 alloc →  9 allocs
//	image      13.6µs →  34.4µs   55Ki → 111Ki    8 allocs → 16 allocs
//
// Text-only is the arm that decides it: almost every auto-routed request
// carries no media, and on that request the shipped filter does nearly nothing
// — a bitmask test skips all four modality dimensions and the floor pass hands
// back the input slice untouched. A per-candidate string predicate cannot skip
// anything, so it pays for the whole pool to answer a question the request
// never asked. Routing is on every request; +255% there is not a refactor, it
// is a regression with a tidier call graph.
//
// So there are two implementations, and what is NOT optional is that they
// agree. Agreement is asserted directly, over the REAL shipped catalog rather
// than a hand-built pool that would only contain the cases I already thought
// of — plus two synthetic rows the catalog does not contain, added because
// mutations proved the catalog sweep alone could not see the rules it claimed
// to pin.
//
// One licensed difference, asserted rather than waved through: on an ACCEPTANCE
// dimension, filterByCapability skips a dimension that would empty the
// candidate pool, because that emptiness is evidence about the CATALOGUE — no
// row declares `file`, and enforcing it would refuse every document request.
// Accepts has no pool — it judges one model for a recovery list a primary
// already backstops — so it reads the same evidence from the snapshot instead.
//
// The FLOOR is not part of that licence and neither side relaxes it. A floor
// says this candidate needs something the request does not carry, which stays
// true however many candidates share it; a pool of only audio-requiring models
// genuinely cannot serve a text request. filterByCapability did relax it once,
// on the acceptance dimensions' reasoning, and that was the divergence this
// file exists to catch reaching the one dimension the licence never covered.
//
// Every disagreement must be explained by a skipped acceptance dimension; one
// that is not is a genuine divergence and fails.

import (
	"fmt"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	core "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// modalityCases enumerates every combination of the four media modalities a
// request can carry, as the bitmask this package uses and the string slice the
// resolver uses. reqText rides on all of them: every chat request carries text,
// and a case without it would be measuring a request that cannot exist.
func modalityCases() []struct {
	name    string
	bits    requestModalities
	carried []string
} {
	dims := []struct {
		bit  requestModalities
		name string
	}{
		{reqImage, modalityImage},
		{reqAudio, modalityAudio},
		{reqVideo, modalityVideo},
		{reqFile, modalityFile},
	}
	var out []struct {
		name    string
		bits    requestModalities
		carried []string
	}
	for mask := range 1 << len(dims) {
		bits := reqText
		carried := []string{"text"}
		name := "text"
		for i, d := range dims {
			if mask&(1<<i) != 0 {
				bits |= d.bit
				carried = append(carried, d.name)
				name += "+" + d.name
			}
		}
		out = append(out, struct {
			name    string
			bits    requestModalities
			carried []string
		}{name, bits, carried})
	}
	return out
}

// snapshotOf builds the resolver's view of the same rows this package filters,
// so both sides are judging the identical catalog. The store.Model round-trip
// is deliberate: it is the conversion production performs, and building
// ModelCapability directly would skip whatever ParseModelCapability does to the
// modality lists on the way through.
func snapshotOf(rows []core.SmartModelRow) *capability.Snapshot {
	models := make([]store.Model, 0, len(rows))
	for _, r := range rows {
		models = append(models, store.Model{
			ID:                 r.ModelID,
			Code:               r.ModelCode,
			ProviderID:         r.ProviderID,
			Type:               "chat",
			Enabled:            true,
			InputModalities:    r.InputModalities,
			RequiredModalities: r.RequiredModalities,
		})
	}
	return capability.NewSnapshot(models)
}

func TestCapabilityPredicates_AgreeOverTheShippedCatalog(t *testing.T) {
	rows := loadShippedCatalogTB(t)
	snap := snapshotOf(rows)

	for _, tc := range modalityCases() {
		t.Run(tc.name, func(t *testing.T) {
			kept, _, skipped := filterByCapability(rows, false, false, false, tc.bits, modsOf(tc.bits))

			keptSet := map[string]bool{}
			for _, r := range kept {
				keptSet[r.ModelID] = true
			}

			var acceptOnly, filterOnly []string
			for _, r := range rows {
				byAccepts := snap.Accepts(r.ModelID, tc.carried)
				byFilter := keptSet[r.ModelID]
				switch {
				case byAccepts && !byFilter:
					acceptOnly = append(acceptOnly, r.ModelID)
				case byFilter && !byAccepts:
					filterOnly = append(filterOnly, r.ModelID)
				}
			}

			// Accepts admitting something the pool filter dropped is never
			// licensed. The pool fail-open only ever WIDENS the filter's
			// result, so this direction has no explanation available.
			if len(acceptOnly) > 0 {
				sort.Strings(acceptOnly)
				t.Errorf("Accepts admits %d model(s) filterByCapability drops: %v\n"+
					"  the resolver would keep a recovery target this package would "+
					"never have selected", len(acceptOnly), truncate(acceptOnly))
			}

			// The other direction is licensed ONLY by a pool-emptying skip,
			// and only for the dimension that was skipped.
			if len(filterOnly) > 0 && len(skipped) == 0 {
				sort.Strings(filterOnly)
				t.Errorf("filterByCapability keeps %d model(s) Accepts rejects, "+
					"with NO dimension skipped: %v\n"+
					"  nothing explains the difference — the two predicates have "+
					"diverged", len(filterOnly), truncate(filterOnly))
			}
			for _, id := range filterOnly {
				if !explainedBySkip(snap, id, tc.carried, skipped) {
					t.Errorf("filterByCapability keeps %s, Accepts rejects it, and the "+
						"skipped dimensions %v do not explain why", id, skipped)
				}
			}
		})
	}
}

// explainedBySkip reports whether removing the skipped dimensions from the
// request's carriage makes Accepts agree. That is precisely what a pool-
// emptying skip means: the filter stopped asking about that modality, so the
// comparison must stop asking too.
func explainedBySkip(snap *capability.Snapshot, modelID string, carried, skipped []string) bool {
	if len(skipped) == 0 {
		return false
	}
	var reduced []string
	for _, c := range carried {
		drop := false
		for _, s := range skipped {
			if s == c {
				drop = true
			}
		}
		if !drop {
			reduced = append(reduced, c)
		}
	}
	return snap.Accepts(modelID, reduced)
}

func truncate(ids []string) string {
	if len(ids) <= 6 {
		return fmt.Sprint(ids)
	}
	return fmt.Sprintf("%v … (+%d more)", ids[:6], len(ids)-6)
}

// The floor, asserted separately because the agreement test above cannot reach
// it from the shipped catalog: only gpt-audio-* declare RequiredModalities
// today, so a catalog-driven sweep exercises one shape of floor and calls it
// covered. In particular an UNRECOGNISED requirement — a modality name neither
// side has a constant for — must be unsatisfiable on both sides. Clearing it
// silently on either side turns a floor into a no-op, and this package's
// requiredBits returns an all-ones mask specifically to prevent that.
func TestCapabilityPredicates_AgreeOnFloors(t *testing.T) {
	rows := []core.SmartModelRow{
		{ModelID: "needs-audio", ModelCode: "needs-audio", ProviderID: "p",
			InputModalities: []string{"text", "audio"}, RequiredModalities: []string{"audio"}},
		{ModelID: "needs-image", ModelCode: "needs-image", ProviderID: "p",
			InputModalities: []string{"text", "image"}, RequiredModalities: []string{"image"}},
		{ModelID: "needs-unknown", ModelCode: "needs-unknown", ProviderID: "p",
			InputModalities: []string{"text"}, RequiredModalities: []string{"hologram"}},
		{ModelID: "plain", ModelCode: "plain", ProviderID: "p",
			InputModalities: []string{"text", "image", "audio"}},

		// Two rows carrying shapes the shipped catalog does not contain, each
		// added because a mutation proved the sweep above cannot see the rule
		// it is supposed to pin.
		//
		// undeclared: no InputModalities at all. Every chat model in the
		// catalog declares some, so the whole fail-open branch — the one that
		// decides UNDECLARED does not mean text-only — sits unexercised by a
		// catalog-driven comparison. Deleting the branch on either side left
		// the sweep green.
		{ModelID: "undeclared", ModelCode: "undeclared", ProviderID: "p"},
		// image-no-text: declares image and omits text. Every catalog row lists
		// text, so requiring it changes nothing there and the "text is never a
		// discriminator" carve-out is invisible; a mutation removing the
		// carve-out also left the sweep green. A row that omits text is the
		// only fixture that can tell the two behaviours apart.
		{ModelID: "image-no-text", ModelCode: "image-no-text", ProviderID: "p",
			InputModalities: []string{"image"}},
	}
	snap := snapshotOf(rows)

	for _, tc := range modalityCases() {
		t.Run(tc.name, func(t *testing.T) {
			kept, _, skipped := filterByCapability(rows, false, false, false, tc.bits, modsOf(tc.bits))
			keptSet := map[string]bool{}
			for _, r := range kept {
				keptSet[r.ModelID] = true
			}
			for _, r := range rows {
				byAccepts := snap.Accepts(r.ModelID, tc.carried)
				byFilter := keptSet[r.ModelID]
				if byAccepts == byFilter {
					continue
				}
				if byFilter && explainedBySkip(snap, r.ModelID, tc.carried, skipped) {
					continue
				}
				t.Errorf("%s: filterByCapability=%v Accepts=%v (skipped=%v) — "+
					"the floor is read differently by the two predicates",
					r.ModelID, byFilter, byAccepts, skipped)
			}
			// The unrecognised requirement can never be met, on either side.
			if snap.Accepts("needs-unknown", tc.carried) {
				t.Errorf("Accepts satisfied a requirement for %q, a modality no request "+
					"can carry — an unrecognised floor was silently cleared", "hologram")
			}
		})
	}
}

// filterByCapabilityViaAccepts is the collapse this file declines to ship: the
// same pool filter with its per-candidate decision delegated to
// capability.Snapshot.Accepts, so there is literally one definition and one
// loop. Kept as a benchmark arm rather than as production code, following the
// filterByCapabilityLegacy precedent in bench_test.go — a rejected alternative
// is only honestly rejected if the number is reproducible.
//
// The pool-level fail-opens are preserved, because dropping them would change
// behaviour and make the comparison meaningless.
func filterByCapabilityViaAccepts(candidates []core.SmartModelRow, snap *capability.Snapshot,
	carried []string) (kept []core.SmartModelRow, dropped int, skippedDims []string) {
	kept = candidates
	for _, m := range carriedModalities {
		var carries bool
		for _, c := range carried {
			if c == m.name {
				carries = true
			}
		}
		if !carries {
			continue
		}
		var pass []core.SmartModelRow
		for _, c := range kept {
			if snap.Accepts(c.ModelID, []string{"text", m.name}) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, m.name)
			continue
		}
		dropped += len(kept) - len(pass)
		kept = pass
	}
	var pass []core.SmartModelRow
	for _, c := range kept {
		if snap.Accepts(c.ModelID, carried) {
			pass = append(pass, c)
		}
	}
	if len(pass) == 0 {
		skippedDims = append(skippedDims, "required_modalities")
	} else {
		dropped += len(kept) - len(pass)
		kept = pass
	}
	return kept, dropped, skippedDims
}

// The head-to-head. Text-only is the arm that matters: it is what the
// overwhelming majority of auto-routed requests are, and it is the arm where
// the shipped filter does almost nothing — a bitmask test skips every modality
// dimension and the floor pass returns the input slice until the first miss.
// A per-candidate string predicate cannot skip anything, so it pays the whole
// pool on every request that carries no media at all.
func BenchmarkCollapse_TextOnly_Shipped(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filterSinkKept, filterSinkDropped, _ = filterByCapability(rows, false, false, false, reqText, modsOf(reqText))
	}
}

func BenchmarkCollapse_TextOnly_ViaAccepts(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	snap := snapshotOf(rows)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filterSinkKept, filterSinkDropped, _ = filterByCapabilityViaAccepts(rows, snap, []string{"text"})
	}
}

func BenchmarkCollapse_Image_Shipped(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filterSinkKept, filterSinkDropped, _ = filterByCapability(rows, false, false, false, reqText|reqImage, modsOf(reqText|reqImage))
	}
}

func BenchmarkCollapse_Image_ViaAccepts(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	snap := snapshotOf(rows)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filterSinkKept, filterSinkDropped, _ = filterByCapabilityViaAccepts(rows, snap,
			[]string{"text", "image"})
	}
}

// Cost attribution for the rejected collapse. The +255% figure above is only
// useful if it names the right cause, and the first reading of it did not:
// "a per-candidate string predicate is expensive" was the conclusion, while the
// arm being measured also introduced a map lookup per candidate that the
// shipped filter does not perform. This isolates the two.
//
//	mapLookupOnly — Snapshot.Get per candidate and nothing else
//	fieldsOnly    — the same modality decision read off the row already in hand
//
// Whichever dominates is the real reason, and the shape of any honest collapse
// follows from it.
func BenchmarkCollapse_Attribution_MapLookupOnly(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	snap := snapshotOf(rows)
	b.ReportAllocs()
	b.ResetTimer()
	var sink int
	for range b.N {
		for _, r := range rows {
			if snap.Get(r.ModelID) != nil {
				sink++
			}
		}
	}
	_ = sink
}

func BenchmarkCollapse_Attribution_FieldsOnly(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	carried := []string{"text", "image"}
	b.ReportAllocs()
	b.ResetTimer()
	var sink int
	for range b.N {
		for _, r := range rows {
			if acceptsModalitiesProbe(r.InputModalities, r.RequiredModalities, carried) {
				sink++
			}
		}
	}
	_ = sink
}

// acceptsModalitiesProbe is the candidate SHARED predicate: the same decision
// as Snapshot.Accepts, taking the two declared lists directly instead of a
// model id to look up. If this is what the collapse costs, the collapse is
// affordable and the duplicate should go.
func acceptsModalitiesProbe(inputModalities, requiredModalities, carried []string) bool {
	if len(inputModalities) > 0 {
		for _, want := range carried {
			if want == "text" {
				continue
			}
			if !containsFeature(inputModalities, want) {
				return false
			}
		}
	}
	for _, need := range requiredModalities {
		if !containsFeature(carried, need) {
			return false
		}
	}
	return true
}

// The collapse that was never measured: keep this package's loop structure
// EXACTLY as it is — bitmask dimension skip, pool-emptying fail-open, the
// prefix-preserving floor scan — and replace only the per-candidate DECISION
// with the shared predicate. That is what "one definition, not one loop" has
// to mean, and the first attempt did not do it: it rebuilt the candidate slice
// per dimension, which is where the cost turned out to live.
func filterByCapabilitySharedPredicate(candidates []core.SmartModelRow, needsTools bool,
	have requestModalities, carried []string) (kept []core.SmartModelRow, dropped int, skippedDims []string) {
	kept = candidates
	for _, m := range carriedModalities {
		if have&m.bit == 0 {
			continue
		}
		var pass []core.SmartModelRow
		for _, c := range kept {
			if acceptsModalitiesProbe(c.InputModalities, nil, []string{m.name}) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, m.name)
			continue
		}
		dropped += len(kept) - len(pass)
		kept = pass
	}
	first := -1
	for i := range kept {
		if !acceptsModalitiesProbe(nil, kept[i].RequiredModalities, carried) {
			first = i
			break
		}
	}
	if first >= 0 {
		pass := make([]core.SmartModelRow, first, len(kept))
		copy(pass, kept[:first])
		for _, c := range kept[first+1:] {
			if acceptsModalitiesProbe(nil, c.RequiredModalities, carried) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, "required_modalities")
		} else {
			dropped += len(kept) - len(pass)
			kept = pass
		}
	}
	if needsTools {
		var pass []core.SmartModelRow
		for _, c := range kept {
			if len(c.Features) == 0 || containsFeature(c.Features, featureFunctionCalling) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, featureFunctionCalling)
		} else {
			dropped += len(kept) - len(pass)
			kept = pass
		}
	}
	return kept, dropped, skippedDims
}

func BenchmarkCollapse_TextOnly_SharedPredicate(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	carried := []string{"text"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filterSinkKept, filterSinkDropped, _ = filterByCapabilitySharedPredicate(rows, false, reqText, carried)
	}
}

func BenchmarkCollapse_Image_SharedPredicate(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	carried := []string{"text", "image"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filterSinkKept, filterSinkDropped, _ = filterByCapabilitySharedPredicate(rows, false, reqText|reqImage, carried)
	}
}

// modsOf derives the string form of a modality bitmask, so a test that already
// says what the request carries does not have to say it twice and cannot get
// the two spellings out of step.
func modsOf(have requestModalities) []string {
	out := []string{"text"}
	for _, m := range carriedModalities {
		if have&m.bit != 0 {
			out = append(out, m.name)
		}
	}
	return out
}
