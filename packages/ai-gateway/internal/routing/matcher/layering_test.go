package matcher

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// matchingSources are the files that answer "does this rule apply to this
// request?". They are the subject of this gate.
//
// enumerate.go is excluded and is not matching: it is the read-only walker
// behind simulate / explain, which resolves every branch a config could take so
// an operator can see the distribution. Resolving targets is its whole job, so
// a rule forbidding catalogue access would forbid the file.
func matchingSources(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var out []string
	for _, f := range all {
		if strings.HasSuffix(f, "_test.go") || f == "enumerate.go" {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no matching sources found — the gate is looking at the wrong directory, or " +
			"every file was excluded, and it would pass no matter what the code did")
	}
	return out
}

// TestMatcher_CannotReachTheCatalogue asserts on the import graph.
//
// A match condition answers "does this rule apply to this request?" from
// request facts alone — the model the caller named, the headers, the key. It
// must not consult the catalogue, because a rule that matches on what a model
// turned out to BE stops being a routing decision and becomes a second, hidden
// capability filter: the rule silently stops matching when a model is disabled,
// re-tagged, or narrowed away by a key's allowlist, and the admin who wrote the
// condition sees a rule that "just doesn't fire" with nothing to read.
//
// Both surfaces that do this today — `providers` and `modelTypes` — reach their
// facts through the routing context rather than through a query, so this gate
// does not catch them; they are a published-contract fix. What it catches is
// the cheaper mistake: someone adding a catalogue lookup to a matcher because
// the fact they wanted was one query away.
func TestMatcher_CannotReachTheCatalogue(t *testing.T) {
	// The catalogue: providers, models, rules as stored. Reading these is the
	// filters' job, after matching has already chosen a rule.
	forbidden := "ai-gateway/internal/platform/store"

	for _, f := range matchingSources(t) {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(path, forbidden) {
				t.Errorf("%s imports the catalogue (%s). A match condition that queries the "+
					"catalogue becomes a capability filter wearing a rule's name: it stops "+
					"firing when a model is disabled or narrowed away, and the admin sees a "+
					"rule that silently never applies. Read the fact from the routing context, "+
					"or make it a filter.", f, path)
			}
		}
	}
}

// TestMatcher_CannotReachTheCatalogueThroughTheRoutingContextEither closes the
// seam the import check cannot see.
//
// The import gate assumed the catalogue was only reachable by importing the
// store package. It is not: the routing context now carries the prepared model
// pool, and `core.TargetLookup` is a function value, so a matcher can read
// catalogue rows or resolve a target without importing anything new. Both
// arrived in this rebuild — the pool to make the catalogue a single read, the
// lookup to resolve leaves — which is exactly how a boundary gets quietly
// re-crossed: the fact becomes convenient before anyone re-reads the rule.
//
// The consequence is identical either way. A condition matching on `ModelPool`
// starts depending on which models happen to be enabled and which the caller's
// key is allowed to see, so the same rule fires for one key and not another,
// and the difference appears nowhere the admin can read.
func TestMatcher_CannotReachTheCatalogueThroughTheRoutingContextEither(t *testing.T) {
	// Catalogue-shaped names a matcher must not touch. Not an import list —
	// these all live in packages matching legitimately depends on.
	forbidden := map[string]string{
		"ModelPool":     "the prepared catalogue rows on the routing context",
		"SmartModelRow": "a catalogue row",
		"TargetLookup":  "the catalogue-backed target resolver",
		"SmartStore":    "the catalogue enumeration port",
	}
	comment := regexp.MustCompile(`(?m)^\s*//.*$`)

	for _, f := range matchingSources(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(raw)
		// Comments legitimately name these — the reasoning for a rule often
		// says what it must not read. Only code counts.
		code := comment.ReplaceAllString(src, "")
		for name, what := range forbidden {
			if strings.Contains(code, name) {
				t.Errorf("%s reads %s (%s). Matching answers a question about the REQUEST; "+
					"reaching the catalogue makes the same rule fire for one key and not "+
					"another, with nothing recorded to say why.", f, name, what)
			}
		}
	}
}
