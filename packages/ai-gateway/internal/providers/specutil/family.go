package specutil

import (
	"strconv"
	"strings"
)

// MatchesFamily reports whether modelID names a member of the given model
// family: the id IS the family, or continues it after a version separator
// ('-' or '.').
//
// Every vendor gates parameters on the model identifier rather than on a
// discoverable capability flag, so the adapters decide by family name. A
// bare string prefix is the wrong test for that decision: "gpt-5.40" starts
// with "gpt-5.4" and "claude-20-..." starts with "claude-2", so a prefix
// check hands a future family whatever carve-out its textual ancestor
// earned. The carve-outs are the ACCEPTS side of each vendor's list, so
// inheriting one is the fail-unsafe direction — the family's parameters are
// forwarded on a guess and the upstream answers 400.
//
// This lives in specutil rather than in an adapter because it decides
// nothing about any wire: it is string matching that three vendors' quirk
// tables happen to need, and provider-adapter-architecture.md §3a Rule 3
// keeps the QUIRKS in the adapters, not the string helper they share.
// GenerationAtLeast reports whether modelID names generation `min` or later
// of a vendor line, where the line is `prefix` followed by a decimal
// generation number: GenerationAtLeast("gpt-6-mini", "gpt-", 5) is true,
// GenerationAtLeast("gpt-4o", "gpt-", 5) is false.
//
// Quirk rules that name today's generations freeze on the day they are
// written, and the generation that has not shipped yet is precisely the one
// nobody has probed. Testing the NUMBER rather than the string puts an
// unreleased generation on the same side as its siblings, which is the side
// that fails safe for every rule using this: a stripped parameter, a widened
// structural fix. Each vendor still owns its own line — the prefix is the
// caller's, and nothing here decides what happens to a match.
//
// A trailing non-numeric (an id that is the bare prefix, or continues with a
// letter) is not a generation and answers false.
func GenerationAtLeast(modelID, prefix string, min int) bool {
	rest, ok := strings.CutPrefix(modelID, prefix)
	if !ok {
		return false
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return false
	}
	gen, err := strconv.Atoi(rest[:digits])
	if err != nil {
		// A digit run long enough to overflow is a generation far past any
		// minimum; read it as "at least".
		return true
	}
	return gen >= min
}

func MatchesFamily(modelID, family string) bool {
	if len(modelID) < len(family) || modelID[:len(family)] != family {
		return false
	}
	if len(modelID) == len(family) {
		return true
	}
	switch modelID[len(family)] {
	case '-', '.':
		return true
	}
	return false
}
