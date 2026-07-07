package matcher

import "testing"

// TestMaxMatchBytes pins the conservative upper-bound walk that sizes the Model-A
// flush-before-deliver lookahead. Under-estimation reopens the boundary leak, so the
// bound must be ≥ the true longest match for every BOUNDED pattern, and every
// UNBOUNDED repeat (*, +, {n,}) must report bounded=false.
func TestMaxMatchBytes(t *testing.T) {
	const utfMax = 4
	cases := []struct {
		name        string
		expr        string
		wantBounded bool
		// wantMin is a lower bound the result must meet/exceed when bounded (the walk
		// over-estimates char classes at utf8.UTFMax, so we assert ≥ the byte length of
		// the longest ASCII match, which it must never go below).
		wantMin int
	}{
		{"literal", "abc", true, 3},
		{"char class one", "[0-9]", true, 1},
		{"fixed repeat card", `[0-9]{16}`, true, 16},
		// Go regexp caps a single repeat at 1000; a long BOUNDED match is a CONCAT of
		// repeats (the only way past the 4096 floor) — the medium-2 long-pattern case.
		{"long bounded concat (medium-2)", `[A-Za-z0-9]{1000}[A-Za-z0-9]{200}`, true, 1200},
		{"bounded range", `[a-z]{1,64}`, true, 64},
		{"two-sided range", `[a-z]{2,24}`, true, 24},
		{"concat", `abc[0-9]{16}`, true, 3 + 16},
		{"alternate takes max", `(abc|[0-9]{16})`, true, 16},
		{"quest one occurrence", "a?", true, 0},
		{"anchored literal", "^abc$", true, 3},
		{"optional then fixed", `x?[0-9]{8}`, true, 8},
		// A surrogate rune has utf8.RuneLen == -1; runeBytes falls back to utf8.UTFMax so
		// the bound never under-counts.
		{"surrogate literal conservative", `\x{d800}`, true, 4},

		{"star unbounded", "a*", false, 0},
		{"plus unbounded", "a+", false, 0},
		{"open repeat unbounded", `[a-z]{20,}`, false, 0},
		{"concat with star unbounded", `abc.*def`, false, 0},
		{"alternate with plus unbounded", `(abc|x+)`, false, 0},
		{"bounded repeat over unbounded sub", `(a*){2,3}`, false, 0},
		{"parse error unbounded", `[unterminated`, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, bounded := MaxMatchBytes(c.expr)
			if bounded != c.wantBounded {
				t.Fatalf("MaxMatchBytes(%q) bounded = %v, want %v (n=%d)", c.expr, bounded, c.wantBounded, n)
			}
			if !bounded {
				return
			}
			// The walk counts each class char at utf8.UTFMax, so the bound must be ≥ the
			// ASCII-byte length (utfMax× for class-heavy patterns). Never below wantMin.
			if n < c.wantMin {
				t.Fatalf("MaxMatchBytes(%q) = %d, want ≥ %d (must not under-estimate)", c.expr, n, c.wantMin)
			}
			// And never absurdly large: a finite bound for these stays within utfMax× the
			// char count (sanity that we didn't return a runaway).
			if n > c.wantMin*utfMax+utfMax {
				t.Fatalf("MaxMatchBytes(%q) = %d, unexpectedly large vs wantMin %d", c.expr, n, c.wantMin)
			}
		})
	}
}

// TestMaxMatchBytes_TightByteWidth pins that ASCII constructs bound to their EXACT byte
// length (1 byte/char), not a blanket utf8.UTFMax (4×) — a 4× inflation would push a
// realistic ASCII rule set toward the tail window and degrade the batching guard, while a
// genuinely multi-byte class member is still counted at its real (upper-bound) UTF-8 width.
func TestMaxMatchBytes_TightByteWidth(t *testing.T) {
	if n, _ := MaxMatchBytes(`[a-z]{100}`); n != 100 {
		t.Fatalf("ascii class {100} = %d, want exactly 100 (1 byte/char, not 4x)", n)
	}
	if n, _ := MaxMatchBytes(`abcdef`); n != 6 {
		t.Fatalf("ascii literal = %d, want 6", n)
	}
	// U+1000–U+1010 are 3-byte runes; one char from the class is counted at 3 (its real
	// width, an exact upper bound), not 4.
	if n, _ := MaxMatchBytes(`[\x{1000}-\x{1010}]{10}`); n != 30 {
		t.Fatalf("3-byte-rune class {10} = %d, want 30 (3 bytes/char)", n)
	}
}

// TestMaxMatchBytes_CaseFoldedLiteral pins the fold-orbit fix: Go stores a (?i) literal as a
// single OpLiteral carrying only the orbit's MINIMUM rune, yet it matches any orbit member, so
// the bound must take the widest member or it under-counts (the reopened-boundary-leak mode the
// first tightening introduced). (?i)k matches U+212A (Kelvin, 3B); (?i)password matches paſſword
// (long-s U+017F ×2). A non-wide orbit stays 1 byte/char.
func TestMaxMatchBytes_CaseFoldedLiteral(t *testing.T) {
	if n, b := MaxMatchBytes(`(?i)k`); !b || n != 3 {
		t.Fatalf("(?i)k = %d bounded=%v, want 3 (Kelvin sign U+212A)", n, b)
	}
	if n, b := MaxMatchBytes(`(?i)password`); !b || n < 10 {
		t.Fatalf("(?i)password = %d bounded=%v, want >= 10 (long-s ×2 ⇒ paſſword)", n, b)
	}
	if n, _ := MaxMatchBytes(`(?i)abc`); n != 3 {
		t.Fatalf("(?i)abc = %d, want 3 (no wide fold members)", n)
	}
}

// TestMaxBoundedMatchBytes pins the aggregate: the largest finite bound across exprs,
// and that ANY unbounded expr is flagged (so the caller discloses it best-effort).
func TestMaxBoundedMatchBytes(t *testing.T) {
	// Mixed: a short PII pattern, a long bounded CONCAT pattern, and an unbounded one.
	maxB, anyUnb := MaxBoundedMatchBytes([]string{
		`[0-9]{16}`,                          // 64 bytes bounded
		`[A-Za-z0-9]{1000}[A-Za-z0-9]{1000}`, // 2000 chars bounded — the long-pattern case
		`[A-Za-z0-9+/=]{20,}`,                // unbounded (JWT/base64 blob) → best-effort
	})
	if maxB < 2000 {
		t.Fatalf("MaxBoundedMatchBytes max = %d, want ≥ 2000 chars (the long bounded concat)", maxB)
	}
	if !anyUnb {
		t.Fatal("MaxBoundedMatchBytes anyUnbounded = false, want true (the {20,} pattern is unbounded)")
	}

	// All bounded, all short → small max, no unbounded.
	maxB2, anyUnb2 := MaxBoundedMatchBytes([]string{`[0-9]{16}`, `abc`})
	if anyUnb2 {
		t.Fatal("anyUnbounded = true for all-bounded exprs")
	}
	if maxB2 == 0 || maxB2 > 64+8 {
		t.Fatalf("max = %d, want a small bounded value (~64)", maxB2)
	}

	// All unbounded → max 0, anyUnbounded true.
	maxB3, anyUnb3 := MaxBoundedMatchBytes([]string{`a*`, `b+`})
	if maxB3 != 0 || !anyUnb3 {
		t.Fatalf("all-unbounded: max=%d anyUnbounded=%v, want 0/true", maxB3, anyUnb3)
	}

	// Empty input → 0, false.
	if m, u := MaxBoundedMatchBytes(nil); m != 0 || u {
		t.Fatalf("empty: max=%d anyUnbounded=%v, want 0/false", m, u)
	}
}
