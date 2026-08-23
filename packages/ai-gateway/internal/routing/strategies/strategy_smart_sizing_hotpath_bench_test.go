package strategies

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// Absolute-cost measurement for filterByCapability over the REAL shipped
// catalog, not a synthetic pool.
//
// The existing bench_test.go arms use a 50-row hand-built pool where half the
// rows take images and none declare a floor. That pool cannot show what the
// four-modality loop costs, because the loop's cost depends on how many
// modalities the request carries (dimensions the request does not carry are
// skipped by a bitmask test) and on how many rows declare a floor (the floor
// pass returns the input slice untouched until the first miss). Both of those
// are properties of the catalog. So this loads tools/db-migrate/model-catalog.json.
//
// The claim under test: the filter now loops 4 modality dimensions instead of 1
// per routing decision over the whole candidate pool.

var (
	filterSinkKept    []core.SmartModelRow
	filterSinkDropped int
)

// loadShippedCatalogTB is loadShippedCatalog widened to testing.TB so a
// benchmark can use it. Deliberately not a change to the existing helper.
func loadShippedCatalogTB(tb testing.TB) []core.SmartModelRow {
	tb.Helper()
	path := filepath.Join("..", "..", "..", "..", "..", "tools", "db-migrate", "model-catalog.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read shipped catalog: %v (looked at %s)", err, path)
	}
	var cf catalogFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		tb.Fatalf("parse shipped catalog: %v", err)
	}
	var rows []core.SmartModelRow
	for _, p := range cf.Providers {
		for _, m := range p.Models {
			if m.Type != "chat" {
				continue
			}
			rows = append(rows, core.SmartModelRow{
				ModelID:            p.AdapterType + "/" + m.Code,
				ModelCode:          m.Code,
				ProviderID:         p.AdapterType,
				InputModalities:    m.InputModalities,
				RequiredModalities: m.RequiredModalities,
				Features:           m.Features,
				MaxContextTokens:   m.MaxContextTokens,
			})
		}
	}
	if len(rows) == 0 {
		tb.Fatal("shipped catalog yielded no chat models; the benchmark would measure an " +
			"empty pool and report a number that means nothing")
	}
	return rows
}

// singleModalityFilter is the pre-change shape: the acceptance question asked
// about images ONLY, gated on an estImages counter. Everything else — the floor
// pass and the tool pass — is identical, so the delta is the three added
// dimensions and the bitmask test that guards them.
func singleModalityFilter(candidates []core.SmartModelRow, needsTools, needsImage bool, have requestModalities) (kept []core.SmartModelRow, dropped int, skippedDims []string) {
	kept = candidates
	if needsImage {
		var pass []core.SmartModelRow
		for _, c := range kept {
			if len(c.InputModalities) == 0 || containsFeature(c.InputModalities, modalityImage) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, modalityImage)
		} else {
			dropped += len(kept) - len(pass)
			kept = pass
		}
	}
	if i := firstFloorMiss(kept, modsOf(have)); i >= 0 {
		pass := make([]core.SmartModelRow, i, len(kept))
		copy(pass, kept[:i])
		for _, c := range kept[i+1:] {
			if len(c.RequiredModalities) == 0 || requiredBits(c.RequiredModalities)&^have == 0 {
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

// requiredBits is the PRE-CHANGE floor test, kept here with the arm that
// models the pre-change filter. It left production when the floor became one
// shared predicate (capability.DeclarationMeetsFloor); a bitmask helper sitting
// in the production file with only a benchmark calling it reads as live code.
func requiredBits(required []string) requestModalities {
	var out requestModalities
	for _, r := range required {
		if r == "text" {
			out |= reqText
			continue
		}
		b := modalityBit(r)
		if b == 0 {
			return ^requestModalities(0) // unknown: unsatisfiable, never absent
		}
		out |= b
	}
	return out
}

func benchFilter(b *testing.B, rows []core.SmartModelRow, needsTools bool, have requestModalities) {
	kept, _, _ := filterByCapability(rows, needsTools, false, false, have, modsOf(have))
	if len(kept) == 0 {
		b.Fatal("pool emptied; the arm measures a degenerate case")
	}
	b.ReportAllocs()
	for b.Loop() {
		filterSinkKept, filterSinkDropped, _ = filterByCapability(rows, needsTools, false, false, have, modsOf(have))
	}
}

func BenchmarkFilterShipped_TextOnly(b *testing.B) {
	benchFilter(b, loadShippedCatalogTB(b), false, reqText)
}
func BenchmarkFilterShipped_TextTools(b *testing.B) {
	benchFilter(b, loadShippedCatalogTB(b), true, reqText)
}
func BenchmarkFilterShipped_Image(b *testing.B) {
	benchFilter(b, loadShippedCatalogTB(b), false, reqText|reqImage)
}
func BenchmarkFilterShipped_ImageTools(b *testing.B) {
	benchFilter(b, loadShippedCatalogTB(b), true, reqText|reqImage)
}

// The worst case for the modality loop: a request carrying every modality, so
// no dimension is skipped by the bitmask test.
func BenchmarkFilterShipped_AllModalities(b *testing.B) {
	benchFilter(b, loadShippedCatalogTB(b), true, reqText|reqImage|reqAudio|reqVideo|reqFile)
}

// The before/after pair on the same shipped pool.
func BenchmarkFilterShipped_TextOnly_SingleModality(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	if kept, _, _ := singleModalityFilter(rows, false, false, reqText); len(kept) == 0 {
		b.Fatal("pool emptied")
	}
	b.ReportAllocs()
	for b.Loop() {
		filterSinkKept, filterSinkDropped, _ = singleModalityFilter(rows, false, false, reqText)
	}
}

func BenchmarkFilterShipped_Image_SingleModality(b *testing.B) {
	rows := loadShippedCatalogTB(b)
	if kept, _, _ := singleModalityFilter(rows, false, true, reqText|reqImage); len(kept) == 0 {
		b.Fatal("pool emptied")
	}
	b.ReportAllocs()
	for b.Loop() {
		filterSinkKept, filterSinkDropped, _ = singleModalityFilter(rows, false, true, reqText|reqImage)
	}
}

// The pool size and the survivor counts are reported, because a ns/op over a
// pool of unknown size is not a measurement of anything.
func TestShippedPoolShapeForTheBenchmarks(t *testing.T) {
	rows := loadShippedCatalogTB(t)
	withFloor, withImage := 0, 0
	for _, r := range rows {
		if len(r.RequiredModalities) > 0 {
			withFloor++
		}
		if containsFeature(r.InputModalities, modalityImage) {
			withImage++
		}
	}
	textKept, _, _ := filterByCapability(rows, false, false, false, reqText, modsOf(reqText))
	imgKept, _, imgSkipped := filterByCapability(rows, false, false, false, reqText|reqImage, modsOf(reqText|reqImage))
	t.Logf("shipped chat pool = %d rows; %d declare a floor; %d declare image input",
		len(rows), withFloor, withImage)
	t.Logf("text-only keeps %d; image keeps %d (skipped dims: %v)",
		len(textKept), len(imgKept), imgSkipped)

	// The single-modality baseline must produce the SAME pool for a text
	// request, or the before/after delta is a comparison of two different
	// amounts of work rather than of the added dimensions.
	legacyKept, _, _ := singleModalityFilter(rows, false, false, reqText)
	if len(legacyKept) != len(textKept) {
		t.Errorf("baseline keeps %d and current keeps %d on a text request; the pair is not "+
			"comparable", len(legacyKept), len(textKept))
	}
}
