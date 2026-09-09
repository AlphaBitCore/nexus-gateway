package matcher

import (
	"strings"
	"testing"
)

// DescribeEngine must agree with the matcher CompileDefault actually returns.
// The pair is split across build-tag files, so the failure mode this guards is a
// build that reports one engine and runs the other — which would make the boot
// log and the policy.matcher introspection source confidently wrong, the exact
// thing they exist to prevent.
func TestDescribeEngine_AgreesWithCompileDefault(t *testing.T) {
	got := DescribeEngine()
	if got.Name == "" {
		t.Fatal("engine name is empty")
	}
	if got.Effect == "" {
		t.Error("engine effect is empty — an operator reading the boot line learns nothing")
	}

	m, bad := CompileDefault([]Pattern{{ID: 1, Expr: `secret-\d+`}})
	if len(bad) != 0 {
		t.Fatalf("a trivially valid pattern failed to compile: %+v", bad)
	}
	switch got.Name {
	case "re2":
		if _, ok := m.(*re2Matcher); !ok {
			t.Errorf("DescribeEngine says re2 but CompileDefault returned %T", m)
		}
		if got.SinglePass {
			t.Error("re2 scans each pattern separately; singlePass must be false")
		}

		if !strings.Contains(got.Effect, "vectorscan") {
			t.Error("the re2 effect should name the build tag that switches engines")
		}
	case "vectorscan":
		if _, ok := m.(*re2Matcher); ok {
			t.Error("DescribeEngine says vectorscan but CompileDefault returned the RE2 matcher")
		}
		if !got.SinglePass {
			t.Errorf("vectorscan is single-pass; got %+v", got)
		}
		// Deliberately NOT asserting a scan-size bound: neither engine caps how
		// much text it examines, which is why the field that claimed one is gone.
		if !strings.Contains(got.Effect, "whole segment") {
			t.Errorf("the vectorscan effect must say it reads the whole segment, got %q", got.Effect)
		}
	default:
		t.Fatalf("unknown engine name %q", got.Name)
	}

	// Whichever engine it is, it has to actually find the pattern — a description
	// of a matcher that does not match is worth nothing.
	//
	// The assertion is MEMBERSHIP, not a hit count. What the pipeline consumes is
	// which (rule, segment) pairs matched; the number of Hit structs is an engine
	// detail. With firstOnly=false a Vectorscan database reports every match END
	// for a pattern, so `secret-\d+` over "secret-42" yields two hits (…-4 and
	// …-42) where RE2 yields one.
	//
	// Requiring exactly one did not merely pin RE2's shape — it was INVERTED: it
	// went red when the pattern was served by the Vectorscan database, the
	// correct state, and green when it degraded to the RE2 residual, the failure
	// state. It survived because the release gate's -run filter never selected
	// this test under the vectorscan tag.
	hits := m.Scan([]string{"leaked secret-42 here"}, false)
	if len(hits) == 0 {
		t.Fatal("Scan found nothing; the matcher this description describes does not match")
	}
	for _, h := range hits {
		if h.ID != 1 || h.Seg != 0 {
			t.Errorf("unexpected hit %+v; every hit must name rule 1 in segment 0", h)
		}
	}
}
