package strategies

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	core "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The acceptance filter is unit-tested against rows this package writes. That
// proves the loop; it cannot say what happens when the loop meets the real
// catalog, and the catalog is the input that changes without anyone editing this
// package.
//
// The defect this guards is the one the filter was written for: routing hands a
// model content it does not accept, and the upstream 400 that follows is OUR
// fault because the caller never chose that model. A synthetic two-row pool can
// only show the mechanism works. A sweep over every model the product ships says
// whether, for each modality a request can actually carry, the pool that
// survives is one every member can serve — or, when no member can, that the
// filter said so out loud instead of narrowing to nothing.
//
// SCOPE, stated because it bounds every verdict below: this reads the REPO
// catalog (tools/db-migrate/model-catalog.json), which seeds a deployment but is
// not guaranteed to equal what any given deployment now serves — a row can be
// added, disabled or edited through the admin API afterwards. So a green run
// here means "the shipped catalog is coherent with the filter", not "production
// is". The production-side answer needs a live inventory, and the failure
// messages say which of the two a reader is looking at.

type catalogModel struct {
	Code               string   `json:"code"`
	Type               string   `json:"type"`
	InputModalities    []string `json:"inputModalities"`
	RequiredModalities []string `json:"requiredModalities"`
	Features           []string `json:"features"`
	MaxContextTokens   *int     `json:"maxContextTokens"`
}

type catalogProvider struct {
	AdapterType string         `json:"adapterType"`
	Name        string         `json:"name"`
	Models      []catalogModel `json:"models"`
}

type catalogFile struct {
	Providers []catalogProvider `json:"providers"`
}

func loadShippedCatalog(t *testing.T) []core.SmartModelRow {
	t.Helper()
	// The package sits at packages/ai-gateway/internal/routing/strategies.
	path := filepath.Join("..", "..", "..", "..", "..", "tools", "db-migrate", "model-catalog.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped catalog: %v (looked at %s)", err, path)
	}
	var cf catalogFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("parse shipped catalog: %v", err)
	}
	var rows []core.SmartModelRow
	for _, p := range cf.Providers {
		for _, m := range p.Models {
			if m.Type != "chat" {
				continue // the acceptance filter runs on the chat routing pool
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
		t.Fatal("shipped catalog yielded no chat models; the parse has drifted from the file " +
			"and this sweep would pass while sweeping nothing")
	}
	return rows
}

// For every modality a request can carry, the surviving pool must be one every
// member can serve — or the filter must have said out loud that it could not
// narrow (fail-open), naming the dimension.
//
// Both outcomes are legitimate; the defect is a third one nobody would notice: a
// pool that narrowed and still contains a model that cannot take the content.
func TestCatalogAcceptance_NoSurvivorIsHandedContentItCannotTake(t *testing.T) {
	rows := loadShippedCatalog(t)

	for _, m := range carriedModalities {
		t.Run(m.name, func(t *testing.T) {
			kept, _, skipped := filterByCapability(rows, false, false, false, reqText|m.bit, modsOf(reqText|m.bit))

			if containsFeature(skipped, m.name) {
				// Fail-open: no candidate declares it, so the dimension was
				// skipped rather than emptying the pool. Correct, and worth
				// naming — this is what happens for "file" today, and it is the
				// reason nothing can route BY document capability.
				declarers := 0
				for _, r := range rows {
					if containsFeature(r.InputModalities, m.name) {
						declarers++
					}
				}
				if declarers != 0 {
					t.Errorf("the %s dimension failed open even though %d shipped model(s) "+
						"declare it — fail-open is for an undeclared capability, not a way to "+
						"skip one the catalog describes", m.name, declarers)
				}
				t.Logf("no shipped chat model declares %q; the dimension fails open, so a "+
					"request carrying it is routed as if the dimension did not exist", m.name)
				return
			}

			if len(kept) == 0 {
				t.Fatalf("the %s dimension emptied the pool without being recorded as skipped; "+
					"every request carrying %s would be unroutable", m.name, m.name)
			}
			var wrong []string
			for _, r := range kept {
				// An empty list is absent catalog metadata, not a claim of
				// text-only, and the filter deliberately keeps such a row.
				if len(r.InputModalities) == 0 {
					continue
				}
				if !containsFeature(r.InputModalities, m.name) {
					wrong = append(wrong, r.ModelID)
				}
			}
			if len(wrong) > 0 {
				sort.Strings(wrong)
				t.Errorf("routing would hand %s content to %d model(s) that do not declare it: %v\n"+
					"The caller never chose these — the upstream 400 that follows is ours.",
					m.name, len(wrong), wrong)
			}
		})
	}
}

// A plain text request must lose ONLY the models whose floor it cannot meet.
// Without this the sweep above could pass by narrowing everything to nothing.
//
// The first version of this asserted the pool was untouched, and the sweep
// immediately reported 163 → 161. That was the test being wrong, not the router:
// the two it drops are the audio-required models, and dropping them from a text
// request is the whole point of the floor. The assertion conflated the
// ACCEPTANCE dimensions (which must not fire when the request carries no media)
// with the FLOOR (which must). Recorded because it is the bucket this program
// keeps landing in — a red arm that is the measurement's fault.
func TestCatalogAcceptance_PlainTextLosesOnlyModelsWithAnUnmetFloor(t *testing.T) {
	rows := loadShippedCatalog(t)
	kept, dropped, skipped := filterByCapability(rows, false, false, false, reqText, modsOf(reqText))

	if len(skipped) != 0 {
		t.Errorf("skipped = %v on a text-only request; no dimension should have to fail open "+
			"when the request carries nothing to narrow by", skipped)
	}

	survived := map[string]bool{}
	for _, r := range kept {
		survived[r.ModelID] = true
	}
	var wronglyDropped []string
	expectDropped := 0
	for _, r := range rows {
		unmetFloor := len(r.RequiredModalities) > 0 &&
			!capability.DeclarationMeetsFloor(r.RequiredModalities, []string{"text"})
		if unmetFloor {
			expectDropped++
			if survived[r.ModelID] {
				t.Errorf("%s requires %v and survived a text-only request — this is the class "+
					"that produced 40 production 400s", r.ModelID, r.RequiredModalities)
			}
			continue
		}
		if !survived[r.ModelID] {
			wronglyDropped = append(wronglyDropped, r.ModelID)
		}
	}
	if len(wronglyDropped) > 0 {
		sort.Strings(wronglyDropped)
		t.Errorf("a text-only request lost %d model(s) with no unmet floor: %v — a media "+
			"dimension is firing on a request that carries no media",
			len(wronglyDropped), wronglyDropped)
	}
	if dropped != expectDropped {
		t.Errorf("dropped = %d, want %d (the models with an unmet floor)", dropped, expectDropped)
	}
	t.Logf("shipped chat pool %d; a text-only request keeps %d and drops %d for an unmet floor",
		len(rows), len(kept), expectDropped)
}

// The floor is the opposite question — does a candidate REQUIRE something the
// request lacks — and it is the one that produced 40 production 400s before it
// was enforced. Sweep it over the shipped catalog: a plain text request must not
// retain a model whose floor it cannot meet.
func TestCatalogAcceptance_FloorRemovesModelsAPlainRequestCannotSatisfy(t *testing.T) {
	rows := loadShippedCatalog(t)
	kept, _, skipped := filterByCapability(rows, false, false, false, reqText, modsOf(reqText))
	if containsFeature(skipped, "required_modalities") {
		t.Skip("every shipped chat model declares a floor a text request cannot meet; " +
			"the fail-open fired and there is nothing to assert")
	}
	var stillThere []string
	for _, r := range kept {
		if len(r.RequiredModalities) == 0 {
			continue
		}
		if !capability.DeclarationMeetsFloor(r.RequiredModalities, []string{"text"}) {
			stillThere = append(stillThere, r.ModelID+" requires "+strings.Join(r.RequiredModalities, "+"))
		}
	}
	if len(stillThere) > 0 {
		sort.Strings(stillThere)
		t.Errorf("a plain text request would still be routed to %d model(s) whose floor it "+
			"cannot meet: %v", len(stillThere), stillThere)
	}
}

// The catalog's own vocabulary must be one the filter understands. A modality
// spelled in a way modalityBit does not recognise is a row the filter silently
// treats as carrying nothing — the row looks configured and is invisible.
func TestCatalogAcceptance_EveryDeclaredModalityIsOneTheFilterKnows(t *testing.T) {
	rows := loadShippedCatalog(t)
	known := map[string]bool{normcore.ModalityImage: true, normcore.ModalityAudio: true,
		normcore.ModalityVideo: true, normcore.ModalityFile: true, "text": true}
	unknown := map[string][]string{}
	for _, r := range rows {
		for _, mod := range r.InputModalities {
			if !known[mod] {
				unknown[mod] = append(unknown[mod], r.ModelID)
			}
		}
		for _, mod := range r.RequiredModalities {
			if !known[mod] {
				unknown[mod+" (required)"] = append(unknown[mod+" (required)"], r.ModelID)
			}
		}
	}
	if len(unknown) > 0 {
		var names []string
		for k := range unknown {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, n := range names {
			t.Errorf("catalog declares modality %q, which the router does not recognise — "+
				"rows: %v. On the acceptance side the filter cannot narrow by it; on the floor "+
				"side requiredBits makes it unsatisfiable, so those models become unroutable.",
				n, unknown[n])
		}
	}
}
