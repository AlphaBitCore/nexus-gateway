package rulepack_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

func findingCodes(l rulepack.PatternLint) map[string]rulepack.LintSeverity {
	m := map[string]rulepack.LintSeverity{}
	for _, f := range l.Findings {
		m[f.Code] = f.Severity
	}
	return m
}

func TestLintPattern_Cases(t *testing.T) {
	cases := []struct {
		name, pattern, flags string
		compiles             bool
		wantCode             string // "" = expect no findings
		wantSeverity         rulepack.LintSeverity
	}{
		{"backreference", `(a)\1`, "", false, "uncompilable", rulepack.LintError},
		{"lookahead", `foo(?=bar)`, "", false, "uncompilable", rulepack.LintError},
		{"bad-flag", `a+`, "x", false, "bad_flag", rulepack.LintError},
		{"bare-dollar", `secret$`, "", true, "anchor_divergence", rulepack.LintWarn},
		{"guarded-dollar", `(?:sh|bash)\s*$`, "", true, "", ""},
		{"multiline-dollar", `^foo$`, "m", true, "", ""},
		{"literal-less-bigrepeat", `[A-Za-z0-9+/]{200,}`, "", true, "no_literal_prefilter", rulepack.LintWarn},
		{"literal-anchored-bigrepeat", `sk-[A-Za-z0-9]{40,}`, "", true, "", ""},
		{"clean-keyword", `ignore\s+previous\s+instructions`, "", true, "", ""},
		// Wide BOUNDED repeat with no >=2-char literal: the original email-PII shape
		// whose flat {1,255} domain measured 246µs. The only literals are the 1-char
		// `@` and `.`, so the prefilter cannot key it — must be flagged.
		{"email-flat-domain", `[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,255}\.[A-Za-z]{2,24}`, "", true, "no_literal_prefilter", rulepack.LintWarn},
		// The restructured, prefilterable email form: widest char-class repeat is
		// exactly {1,64} (<= the threshold) and the domain is a bounded label group,
		// not a flat wide repeat — must NOT trip (measured 1.7µs).
		{"email-structured-domain", `[A-Za-z0-9._%+-]{1,64}@(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,62})\.){1,8}[A-Za-z]{2,24}`, "", true, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := rulepack.LintPattern(c.pattern, c.flags)
			if l.Compiles != c.compiles {
				t.Errorf("Compiles = %v, want %v (findings=%+v)", l.Compiles, c.compiles, l.Findings)
			}
			codes := findingCodes(l)
			if c.wantCode == "" {
				if len(l.Findings) != 0 {
					t.Errorf("expected no findings, got %+v", l.Findings)
				}
				return
			}
			if sev, ok := codes[c.wantCode]; !ok {
				t.Errorf("expected finding %q, got %+v", c.wantCode, l.Findings)
			} else if sev != c.wantSeverity {
				t.Errorf("finding %q severity = %q, want %q", c.wantCode, sev, c.wantSeverity)
			}
		})
	}
}

func TestLintPack_ReturnsPerRuleFindings(t *testing.T) {
	pack := &rulepack.Pack{
		Name: "nexus/x", Version: "v1.0.0", Maintainer: "nexus",
		Rules: []rulepack.Rule{
			{RuleID: "clean", Category: "x", Severity: "soft", Pattern: `ignore\s+previous`},
			{RuleID: "bare-dollar", Category: "x", Severity: "soft", Pattern: `secret$`},
			{RuleID: "backref", Category: "x", Severity: "soft", Pattern: `(a)\1`},
		},
	}
	got := rulepack.LintPack(pack)
	// Only the two rules with findings are returned (clean is omitted).
	if len(got) != 2 {
		t.Fatalf("expected findings for 2 rules, got %d: %+v", len(got), got)
	}
	byID := map[string]rulepack.RuleLint{}
	for _, rl := range got {
		byID[rl.RuleID] = rl
	}
	if _, ok := byID["clean"]; ok {
		t.Errorf("clean rule should have no findings")
	}
	if byID["backref"].Findings[0].Severity != rulepack.LintError {
		t.Errorf("backref should be a lint error, got %+v", byID["backref"].Findings)
	}
	if byID["bare-dollar"].Findings[0].Code != "anchor_divergence" {
		t.Errorf("bare-dollar should warn anchor_divergence, got %+v", byID["bare-dollar"].Findings)
	}
}

// TestLint_AllSeedRules_NoErrors gates that every shipped seed rule at least
// RE2-compiles (no LintError). Warnings (divergence / no-prefilter) are allowed
// and logged — they are advisory, not blocking.
func TestLint_AllSeedRules_NoErrors(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read seed dir: %v", err)
	}
	warns := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		pack, _, err := rulepack.LoadYAML(data)
		if err != nil {
			t.Fatalf("LoadYAML %s: %v", e.Name(), err)
		}
		for _, r := range pack.Rules {
			l := rulepack.LintPattern(r.Pattern, r.Flags)
			for _, f := range l.Findings {
				if f.Severity == rulepack.LintError {
					t.Errorf("%s/%s: lint ERROR %s: %s", pack.Name, r.RuleID, f.Code, f.Message)
				} else {
					warns++
					t.Logf("%s/%s: %s — %s", pack.Name, r.RuleID, f.Code, f.Message)
				}
			}
		}
	}
	t.Logf("seed lint: 0 errors, %d advisory warnings", warns)
}

// TestLintPattern_AnchorAndWideClassBranches exercises the AST-walk helpers
// (isWideClass any-char paths, classIncludesNewline, consumesNewline for
// star/plus/repeat, and hasDivergentEndAnchor recursion through groups and
// alternations) that the simple table above does not reach.
func TestLintPattern_AnchorAndWideClassBranches(t *testing.T) {
	has := func(l rulepack.PatternLint, code string) bool {
		for _, f := range l.Findings {
			if f.Code == code {
				return true
			}
		}
		return false
	}
	cases := []struct {
		name, pattern, flags string
		wantDivergence       bool
		wantNoPrefilter      bool
	}{
		// `.` unbounded over any-char-not-newline, no literal -> slow path.
		{"dot-star-unbounded", `.{50,}`, "", false, true},
		// DOTALL `.` is OpAnyChar (wide incl newline), unbounded, no literal.
		{"dotall-star-unbounded", `.{50,}`, "s", false, true},
		// `$` guarded by a star over a class that includes newline -> no divergence.
		{"guarded-by-newline-class-star", `secret[\s\S]*$`, "", false, false},
		// `$` guarded by a bounded repeat over a newline-including class.
		{"guarded-by-newline-class-repeat", `secret[\x00-\x7f]{2,5}$`, "", false, false},
		// `$` inside an alternation branch within a group -> divergent (recursion).
		{"divergent-in-alternation", `(secretA$|secretB)`, "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := rulepack.LintPattern(c.pattern, c.flags)
			if !l.Compiles {
				t.Fatalf("expected compile; findings=%+v", l.Findings)
			}
			if got := has(l, "anchor_divergence"); got != c.wantDivergence {
				t.Errorf("anchor_divergence = %v, want %v (findings=%+v)", got, c.wantDivergence, l.Findings)
			}
			if got := has(l, "no_literal_prefilter"); got != c.wantNoPrefilter {
				t.Errorf("no_literal_prefilter = %v, want %v (findings=%+v)", got, c.wantNoPrefilter, l.Findings)
			}
		})
	}
}
