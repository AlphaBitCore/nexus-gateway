// Package extract_test pins the behavioral delta between the Tier-2
// PatternNormalizer alone and the full Tier 1+2+3 registry, per input.
//
// This is the safety gate for deleting the Tier-2 standard-API specs
// (normalize-unification Task 1.4): before any spec is removed we need
// on-record proof of what Tier 2 extracts today versus what the full
// registry produces, so the deletion provably loses nothing. The file
// is an external test package (extract_test) because it imports the
// parent normalize package for BuildRegistry, which itself imports
// extract — an in-package test would create an import cycle.
package extract_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// knownPhase1Gaps lists corpus cases where the "registry is never worse
// than Tier 2 alone" invariant currently FAILS. Each entry maps the
// corpus case directory name to the reason the gap exists. The Phase 1
// gate (Task 1.6) requires this map to shrink to empty: every entry is
// a regression the decoder unification must close before the Tier-2
// standard-API specs can be deleted. Failures on cases NOT listed here
// fail the test immediately.
var knownPhase1Gaps = map[string]string{
	// (empty — no corpus case currently violates the invariant)
}

// corpusCaseMeta mirrors the on-disk meta.json schema defined by the
// conformance harness (conformance/harness.go caseMeta): explicit
// camelCase keys mapped onto core.Meta, which itself carries no JSON
// tags. Keep the two structs in lockstep.
type corpusCaseMeta struct {
	AdapterType  string `json:"adapterType"`
	Model        string `json:"model"`
	ContentType  string `json:"contentType"`
	Direction    string `json:"direction"`
	EndpointPath string `json:"endpointPath"`
	Stream       bool   `json:"stream"`
}

// readCorpusMeta loads a corpus case's meta.json into core.Meta,
// failing the test on unreadable or undecodable files so a broken case
// never silently passes the invariant.
func readCorpusMeta(t *testing.T, path string) core.Meta {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var cm corpusCaseMeta
	if err := json.Unmarshal(data, &cm); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return core.Meta{
		AdapterType:  cm.AdapterType,
		Model:        cm.Model,
		ContentType:  cm.ContentType,
		Direction:    core.Direction(cm.Direction),
		EndpointPath: cm.EndpointPath,
		Stream:       cm.Stream,
	}
}

// observation captures the fields the pin compares for one normalizer
// run: did it claim the body, and with what structural result.
type observation struct {
	Claimed      bool      // err == nil (the normalizer claimed the body)
	Kind         core.Kind // payload.Kind
	DetectedSpec string    // payload.DetectedSpec
	Model        string    // payload.Model
	MsgCount     int       // len(payload.Messages)
	HasUsage     bool      // payload.Usage != nil
	Confidence   float64   // payload.Confidence
}

// chainNormalizer is satisfied by both *extract.PatternNormalizer
// (Tier 2 alone) and *core.Registry (the full Tier 1+2+3 chain).
type chainNormalizer interface {
	Normalize(ctx context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error)
}

// observe runs one normalizer over raw+meta and records the comparison
// fields. ErrUnsupported is part of pinned behavior (Claimed=false with
// a partial Confidence), not a test failure.
func observe(t *testing.T, n chainNormalizer, raw []byte, meta core.Meta) observation {
	t.Helper()
	payload, err := n.Normalize(context.Background(), raw, meta)
	return observation{
		Claimed:      err == nil,
		Kind:         payload.Kind,
		DetectedSpec: payload.DetectedSpec,
		Model:        payload.Model,
		MsgCount:     len(payload.Messages),
		HasUsage:     payload.Usage != nil,
		Confidence:   payload.Confidence,
	}
}

// assertObs compares an observation against its pin field by field so a
// drift names the exact field that moved.
func assertObs(t *testing.T, side string, got, want observation) {
	t.Helper()
	if got.Claimed != want.Claimed {
		t.Errorf("%s: Claimed = %v, pinned %v", side, got.Claimed, want.Claimed)
	}
	if got.Kind != want.Kind {
		t.Errorf("%s: Kind = %q, pinned %q", side, got.Kind, want.Kind)
	}
	if got.DetectedSpec != want.DetectedSpec {
		t.Errorf("%s: DetectedSpec = %q, pinned %q", side, got.DetectedSpec, want.DetectedSpec)
	}
	if got.Model != want.Model {
		t.Errorf("%s: Model = %q, pinned %q", side, got.Model, want.Model)
	}
	if got.MsgCount != want.MsgCount {
		t.Errorf("%s: MsgCount = %d, pinned %d", side, got.MsgCount, want.MsgCount)
	}
	if got.HasUsage != want.HasUsage {
		t.Errorf("%s: HasUsage = %v, pinned %v", side, got.HasUsage, want.HasUsage)
	}
	if math.Abs(got.Confidence-want.Confidence) > 1e-9 {
		t.Errorf("%s: Confidence = %v, pinned %v", side, got.Confidence, want.Confidence)
	}
}

// TestCodecParityWithTier2Specs is the Task 0.4 behavior-pin test.
//
// Part 1 (always runs — never vacuous): the two committed browser-capture
// fixtures are pinned to EXACT current values on both sides, Tier-2 alone
// and full registry. Any change to either side shows up as a named-field
// diff here before it ships.
//
// Part 2 (corpus, skipped while ../conformance/corpus/ is empty): for
// every corpus case the test asserts the Phase-1 target invariant —
// the full registry's result is never WORSE than Tier 2 alone:
//
//  1. registry Kind is AI whenever Tier-2 Kind is AI;
//  2. registry MsgCount >= Tier-2 MsgCount;
//  3. registry Usage present whenever Tier-2 Usage present.
//
// Violations are failures unless the case is listed in knownPhase1Gaps,
// in which case the gap is logged; the map must be empty by the Phase 1
// gate (Task 1.6).
func TestCodecParityWithTier2Specs(t *testing.T) {
	tier2 := extract.NewPatternNormalizer()
	registry := normalize.BuildRegistry()

	t.Run("pinned-fixtures", func(t *testing.T) {
		// Meta values mirror the live agent captures the fixtures came
		// from (same values as TestRepro_BrowserCaptures in the parent
		// package) so the pin reflects production-shaped inputs.
		cases := []struct {
			name         string
			file         string
			meta         core.Meta
			wantTier2    observation
			wantRegistry observation
		}{
			{
				name: "chatgptweb-req",
				file: "chatgptweb-req.json",
				meta: core.Meta{
					AdapterType:  "chatgpt-web",
					ContentType:  "application/json",
					Direction:    core.DirectionRequest,
					EndpointPath: "/backend-api/f/conversation",
				},
				// Tier 2 alone claims via the generic multi-spec probe:
				// the chatgpt-web ChatSpec matches model + 1 user message
				// at full probe confidence.
				wantTier2: observation{
					Claimed:      true,
					Kind:         core.KindAIChat,
					DetectedSpec: "pattern:chatgpt-web",
					Model:        "gpt-5-5-thinking",
					MsgCount:     1,
					HasUsage:     false,
					Confidence:   1.0,
				},
				// Full registry: the Tier-1 chatgpt-web adapter claims
				// first (DetectedSpec without the "pattern:" prefix). The
				// spec scores 1.0 on this body so the adapter confidence
				// floor never engages.
				wantRegistry: observation{
					Claimed:      true,
					Kind:         core.KindAIChat,
					DetectedSpec: "chatgpt-web",
					Model:        "gpt-5-5-thinking",
					MsgCount:     1,
					HasUsage:     false,
					Confidence:   1.0,
				},
			},
			{
				name: "claudeweb-req",
				file: "claudeweb-req.json",
				meta: core.Meta{
					AdapterType:  "claude-web",
					ContentType:  "application/json",
					Direction:    core.DirectionRequest,
					EndpointPath: "/api/organizations/91bafac6-6120-4d56-964e-6459c4f7cd5a/chat_conversations/70d7e000-17df-4353-ba7f-f516e8fb0990/completion",
				},
				// Tier 2 alone REJECTS this body: the claude-web spec's
				// single-prompt shape caps its probe confidence at 0.6
				// (0.4 ContentPath + 0.2 signature), below the 0.7 Tier-2
				// threshold, so the probe returns ErrUnsupported with the
				// partial confidence surfaced — exactly the gap host
				// selection evidence covers.
				wantTier2: observation{
					Claimed:      false,
					Kind:         core.KindUnsupported,
					DetectedSpec: "",
					Model:        "",
					MsgCount:     0,
					HasUsage:     false,
					Confidence:   0.6,
				},
				// Full registry: the Tier-1 claude-web adapter claims on
				// host selection evidence, KEEPING the honest 0.6 coverage
				// (no floor) — the SelectionEvidence stamp is what carries
				// it over the threshold, not an inflated number.
				wantRegistry: observation{
					Claimed:      true,
					Kind:         core.KindAIChat,
					DetectedSpec: "claude-web",
					Model:        "claude-opus-4-7",
					MsgCount:     1,
					HasUsage:     false,
					Confidence:   0.6,
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				raw, err := os.ReadFile(filepath.Join("..", "testdata", tc.file))
				if err != nil {
					t.Fatalf("read fixture: %v", err)
				}
				gotTier2 := observe(t, tier2, raw, tc.meta)
				gotRegistry := observe(t, registry, raw, tc.meta)
				t.Logf("tier2:    %+v", gotTier2)
				t.Logf("registry: %+v", gotRegistry)
				assertObs(t, "tier2", gotTier2, tc.wantTier2)
				assertObs(t, "registry", gotRegistry, tc.wantRegistry)
			})
		}
	})

	t.Run("corpus-invariant", func(t *testing.T) {
		dirs, err := filepath.Glob(filepath.Join("..", "conformance", "corpus", "*"))
		if err != nil {
			t.Fatalf("glob corpus: %v", err)
		}
		// A case is ready only when both a non-empty wire and a meta.json
		// are present. The corpus is populated by a separate task lane
		// (0.1-0.3); a dir that exists without both files is mid-landing,
		// not a parity failure — log and move on.
		var caseDirs []string
		for _, dir := range dirs {
			fi, statErr := os.Stat(dir)
			if statErr != nil || !fi.IsDir() {
				continue
			}
			wireInfo, wireErr := os.Stat(filepath.Join(dir, "wire"))
			_, metaErr := os.Stat(filepath.Join(dir, "meta.json"))
			if wireErr != nil || wireInfo.Size() == 0 || metaErr != nil {
				t.Logf("corpus case %s incomplete (wire+meta.json not both present) — not yet pinned", filepath.Base(dir))
				continue
			}
			caseDirs = append(caseDirs, dir)
		}
		if len(caseDirs) == 0 {
			t.Skip("no complete conformance corpus cases yet (Task 0.1-0.3 in flight) — pinned-fixtures subtest still ran")
		}
		gapsHit := map[string]string{}
		for _, dir := range caseDirs {
			name := filepath.Base(dir)
			t.Run(name, func(t *testing.T) {
				raw, err := os.ReadFile(filepath.Join(dir, "wire"))
				if err != nil {
					t.Fatalf("read wire: %v", err)
				}
				meta := readCorpusMeta(t, filepath.Join(dir, "meta.json"))

				gotTier2 := observe(t, tier2, raw, meta)
				gotRegistry := observe(t, registry, raw, meta)
				t.Logf("tier2:    %+v", gotTier2)
				t.Logf("registry: %+v", gotRegistry)

				// The invariant only constrains cases where Tier 2 alone
				// claimed an AI shape: deleting the Tier-2 standard specs
				// must not lose anything Tier 2 extracts today.
				if !gotTier2.Claimed || !gotTier2.Kind.IsAI() {
					t.Logf("tier2 did not claim an AI shape — invariant trivially holds")
					return
				}
				var violations []string
				if !gotRegistry.Kind.IsAI() {
					violations = append(violations, "registry kind "+string(gotRegistry.Kind)+" is not AI while tier2 kind is "+string(gotTier2.Kind))
				}
				if gotRegistry.MsgCount < gotTier2.MsgCount {
					violations = append(violations, "registry message count below tier2")
				}
				if gotTier2.HasUsage && !gotRegistry.HasUsage {
					violations = append(violations, "registry lost usage that tier2 extracted")
				}
				if len(violations) == 0 {
					return
				}
				if reason, listed := knownPhase1Gaps[name]; listed {
					gapsHit[name] = reason
					for _, v := range violations {
						t.Logf("KNOWN PHASE-1 GAP (%s): %s", reason, v)
					}
					return
				}
				for _, v := range violations {
					t.Errorf("invariant violated (case not in knownPhase1Gaps): %s", v)
				}
			})
		}
		// Surface the live gap list in every run so its size is visible
		// in CI logs; it must reach zero by the Phase 1 gate.
		t.Logf("knownPhase1Gaps hit this run: %d of %d listed", len(gapsHit), len(knownPhase1Gaps))
		for name, reason := range knownPhase1Gaps {
			if _, hit := gapsHit[name]; !hit {
				t.Errorf("knownPhase1Gaps entry %q (%s) no longer fails the invariant — remove it from the list", name, reason)
			}
		}
	})
}
