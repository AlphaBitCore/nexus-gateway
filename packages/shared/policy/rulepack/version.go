package rulepack

import "strconv"

// CompareSemver orders two v-prefixed semver strings (e.g. "v1.2.3").
// Returns -1 if a < b, 0 if equal, +1 if a > b. Only the numeric
// major.minor.patch triple is compared; a build/prerelease suffix is
// ignored for ordering (sufficient for "pick the latest release" — packs
// validate to the v-semver shape at import via semverRE). A string that
// does not parse sorts below any that does, and two unparseable strings
// fall back to a plain lexical compare so ordering stays total.
func CompareSemver(a, b string) int {
	am, ok1 := parseSemverTriple(a)
	bm, ok2 := parseSemverTriple(b)
	switch {
	case ok1 && !ok2:
		return 1
	case !ok1 && ok2:
		return -1
	case !ok1 && !ok2:
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	}
	for i := range 3 {
		if am[i] != bm[i] {
			if am[i] < bm[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseSemverTriple extracts [major, minor, patch] from a v-prefixed semver
// using the same regex that validates pack versions at import. ok=false when
// the string is not a recognized v-semver.
func parseSemverTriple(v string) ([3]int, bool) {
	m := semverRE.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range 3 {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}
