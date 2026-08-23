package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPlan_IsAssembledInOneplace is a structural assertion over the whole
// gateway, because the property is about who is allowed to write, and a
// behavioural test can only see what was written.
//
// The plan is one ordered list. Anything that rewrites that list decides what
// the request will reach — the quota downgrade promoting a cheaper model, a
// compatibility filter removing targets an adapter cannot serve. Each of those
// is legitimate; each of them assigning the slice directly is not, because two
// assemblers of the same list diverge in ways nothing can see: both produce a
// list of targets, both look right, and the difference only shows up as a
// request served by a model nobody selected.
//
// This is how the previous version drifted. The downgrade built its own
// reordered slice, and the rule "the cheapest becomes primary, the rest stay as
// fallbacks" lived in the downgrade rather than in the plan — so when the plan
// grew a notion of which half a target belonged to, the downgrade did not know
// and quietly flattened it.
func TestPlan_IsAssembledInOnePlace(t *testing.T) {
	// Mutating methods live here; every other package goes through them.
	root := filepath.Join("..", "..", "..")
	assign := regexp.MustCompile(`\.Dispatch\s*=[^=]`)
	comment := regexp.MustCompile(`(?m)^\s*//.*$`)

	var offenders []string
	seen := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		// A subtree we could not enter is a subtree we did not search, and this
		// gate's whole claim is that it searched all of them. Swallowing the
		// error would let it report no offenders because it never looked.
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The plan's own package owns its shape.
		if strings.Contains(filepath.ToSlash(path), "/routing/core/") {
			return nil
		}
		b, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		seen++
		src := comment.ReplaceAllString(string(b), "")
		// The resolver builds the plan once, in the composite literal that
		// creates it; that is assembly, not rewriting.
		if assign.MatchString(src) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if seen == 0 {
		t.Fatal("no source files were scanned — the gate is pointed at the wrong tree and " +
			"would pass however the plan was rewritten")
	}
	for _, f := range offenders {
		t.Errorf("%s assigns the plan's dispatch list directly. Whatever it is deciding — "+
			"promote, narrow, reorder — belongs on RouteResult as a named method, so the "+
			"plan's shape is known in one place instead of being re-derived by every caller "+
			"that wants to change it.", f)
	}
}
