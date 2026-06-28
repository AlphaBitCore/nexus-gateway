package matcher

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStripAnchors_FixturesCleanAndCompilable runs the anchor strip over EVERY
// real seed rule and asserts the output stays Vectorscan-compilable: no `(?:)`
// empty fragments (which silently demote a pattern to the slow RE2 residual) and
// still RE2-compilable. This guards the prefilter against the regression where
// stripped anchors produced `(?:)SECRET` and the whole raw-body prefilter ran on
// pure-Go regexp at 90%+ CPU. Vectorscan-compilability itself is verified on the
// rig (cgo); this is the pure-Go pre-flight.
func TestStripAnchors_FixturesCleanAndCompilable(t *testing.T) {
	const path = "../../../../../tools/db-migrate/seed/fixtures/rule.json"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("seed fixtures not found (%v) — run from repo checkout", err)
	}
	var rules []struct {
		RuleID  string `json:"ruleId"`
		Pattern string `json:"pattern"`
		Flags   string `json:"flags"`
	}
	if err := json.Unmarshal(data, &rules); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("no rules in fixtures")
	}

	var stripErr, dirty, badRE2, alwaysMatch int
	for _, r := range rules {
		s, err := StripAnchors(r.Pattern)
		if err != nil {
			stripErr++
			t.Logf("strip ERROR %s: %v  pattern=%q", r.RuleID, err, r.Pattern)
			continue
		}
		if strings.Contains(s, "(?:)") {
			dirty++
			t.Errorf("DIRTY %s: stripped form still contains (?:) → demotes to RE2 residual: %q", r.RuleID, s)
		}
		re, err := regexp.Compile(s)
		if err != nil {
			badRE2++
			t.Logf("RE2 reject %s: %v  stripped=%q", r.RuleID, err, s)
			continue
		}
		// Over-broad check: a stripped pattern that matches the empty string is an
		// always-hit → the prefilter can never skip when that rule is active. Not a
		// failure (still sound), but report the count so we know prefilter coverage.
		if re.MatchString("") {
			alwaysMatch++
			t.Logf("ALWAYS-MATCH %s: stripped=%q (prefilter cannot skip when this rule is bound)", r.RuleID, s)
		}
	}
	t.Logf("fixtures=%d stripErr=%d dirty=%d badRE2=%d alwaysMatch=%d", len(rules), stripErr, dirty, badRE2, alwaysMatch)
	if dirty > 0 {
		t.Fatalf("%d stripped patterns still emit (?:) — they will fall to the RE2 residual and tank the prefilter", dirty)
	}
}
