package rulepack_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.2.0", "v1.10.0", -1}, // numeric, not lexical (10 > 2)
		{"v2.0.0", "v1.9.9", 1},
		{"v1.0.0-rc1", "v1.0.0", 0}, // suffix ignored for ordering
		{"v1.0.0", "garbage", 1},    // parseable sorts above unparseable
		{"garbage", "v1.0.0", -1},
		{"aaa", "bbb", -1}, // both unparseable -> lexical
		{"bbb", "aaa", 1},
		{"zzz", "zzz", 0},
	}
	for _, c := range cases {
		if got := rulepack.CompareSemver(c.a, c.b); got != c.want {
			t.Errorf("CompareSemver(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareSemver_OverflowTreatedUnparseable(t *testing.T) {
	// A numeric component too large for int triggers the Atoi error path in
	// parseSemverTriple, so the string is treated as unparseable and sorts
	// below a well-formed version.
	big := "v99999999999999999999.0.0"
	if got := rulepack.CompareSemver(big, "v1.0.0"); got != -1 {
		t.Errorf("overflow version should sort below valid; got %d", got)
	}
}
