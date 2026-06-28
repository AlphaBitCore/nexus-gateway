package rules

import (
	"github.com/goccy/go-json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// TestBuiltinRulesAppearInSeed enforces a minimal lockstep between the Go
// BuiltinRules registry (compile-time invariants) and the seed/fixtures/AlertRule.json
// AlertRule snapshot (admin-tunable runtime defaults).
//
// The architectural intent (alerting-architecture.md §1) is:
//
//	"every builtin's seed counterpart must exist with the binding name;
//	 admins can change params but not delete builtin-shipped rules."
//
// This test enforces one half — every Go BuiltinRules.ID must appear as an
// AlertRule row in the seed. The reverse direction (seed rules not in Go) is
// covered by TestSeedRulesAppearInBuiltin so the admin UI's "Reset Rule"
// button always has a Go-side default to write back.
func TestBuiltinRulesAppearInSeed(t *testing.T) {
	seedIDs := loadSeedAlertRuleIDs(t)
	if len(seedIDs) == 0 {
		t.Fatal("loaded zero AlertRule rows from seed/fixtures/AlertRule.json; locator may be wrong")
	}

	var missing []string
	for _, b := range BuiltinRules {
		if _, ok := seedIDs[b.ID]; !ok {
			missing = append(missing, b.ID)
		}
	}
	if len(missing) > 0 {
		t.Errorf(
			"%d builtin rule(s) defined in Go but not in seed (fresh installs will lack them):\n  %v\n"+
				"Fix: add an AlertRule object for each in tools/db-migrate/seed/fixtures/AlertRule.json.",
			len(missing), missing,
		)
	}
}

// TestSeedRulesAppearInBuiltin enforces the reverse direction of the
// lockstep: every AlertRule row in seed/fixtures/AlertRule.json must have a matching
// RuleDef in BuiltinRules. Drift here means the admin UI's "Reset Rule"
// button silently no-ops for that rule (the reset handler looks up the
// canonical default by ID, and a missing entry leaves the operator-edited
// row in place). 2026-05-15 prod drift incident — three credential.* rules
// shipped to seed by e41-v2 but never reflected in Go — is the canonical
// failure mode this guards against.
func TestSeedRulesAppearInBuiltin(t *testing.T) {
	seedIDs := loadSeedAlertRuleIDs(t)
	if len(seedIDs) == 0 {
		t.Fatal("loaded zero AlertRule rows from seed/fixtures/AlertRule.json; locator may be wrong")
	}

	builtinIDs := make(map[string]struct{}, len(BuiltinRules))
	for _, b := range BuiltinRules {
		builtinIDs[b.ID] = struct{}{}
	}

	var missing []string
	for id := range seedIDs {
		if _, ok := builtinIDs[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf(
			"%d seed AlertRule row(s) lack a matching Go BuiltinRules entry "+
				"(the \"Reset Rule\" admin button will silently no-op for these):\n  %v\n"+
				"Fix: add a RuleDef for each in packages/nexus-hub/internal/alerts/engine/rules/builtin.go.",
			len(missing), missing,
		)
	}
}

// TestBuiltinRuleSourceTypesAreCanonical pins every BuiltinRules.SourceType
// against the canonical set in alerting.AllSourceTypes. A new SourceType in
// builtin.go without the matching addition to AllSourceTypes (and the
// schema.prisma AlertRule.sourceType doc-comment) trips this test — the
// schema comment is the operator-facing contract for what the column may
// hold, so the three layers must stay in lockstep.
func TestBuiltinRuleSourceTypesAreCanonical(t *testing.T) {
	valid := make(map[string]struct{}, len(alerting.AllSourceTypes))
	for _, st := range alerting.AllSourceTypes {
		valid[st] = struct{}{}
	}

	offenders := map[string][]string{}
	for _, b := range BuiltinRules {
		if _, ok := valid[b.SourceType]; !ok {
			offenders[b.SourceType] = append(offenders[b.SourceType], b.ID)
		}
	}
	if len(offenders) > 0 {
		var sortedTypes []string
		for st := range offenders {
			sortedTypes = append(sortedTypes, st)
		}
		sort.Strings(sortedTypes)
		t.Errorf(
			"%d builtin rule SourceType value(s) not in alerting.AllSourceTypes: %v\n"+
				"Fix: add the value to AllSourceTypes in packages/nexus-hub/internal/alerts/engine/types.go "+
				"AND update the AlertRule.sourceType doc-comment in tools/db-migrate/schema/observability.prisma.",
			len(sortedTypes), offenders,
		)
	}
}

func loadSeedAlertRuleIDs(t *testing.T) map[string]struct{} {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate seed fixture")
	}
	// thisFile = .../packages/nexus-hub/internal/alerts/engine/rules/builtin_seed_lockstep_test.go
	// 6 levels up from rules/ reaches the repo root (rules → engine → alerts → internal → nexus-hub → packages → repo).
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "..", "..")
	fixturePath := filepath.Join(repoRoot, "tools", "db-migrate", "seed", "fixtures", "AlertRule.json")

	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read AlertRule fixture %s: %v", fixturePath, err)
	}

	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("parse AlertRule fixture %s: %v", fixturePath, err)
	}

	ids := map[string]struct{}{}
	for _, r := range rows {
		if r.ID != "" {
			ids[r.ID] = struct{}{}
		}
	}
	return ids
}
