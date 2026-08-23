package executor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExecutor_DoesNotKnowStrategyTypes is a structural assertion, read off the
// source because that is where the property lives.
//
// The recovery engine reads a plan and an error class. It does not know how the
// plan was produced — whether a router LLM chose it, a weighted draw landed on
// it, or an admin named it outright. That separation is what lets one recovery
// engine serve every strategy, and what stopped the executor from growing a
// special case per strategy the way the resolver once did.
//
// A behavioural test cannot catch the drift: the first `if strategyType ==
// "smart"` would pass every existing test and simply add a branch. The failure
// mode is a slow one — one special case, then another — so the assertion is on
// the shape rather than on an outcome.
func TestExecutor_DoesNotKnowStrategyTypes(t *testing.T) {
	// The vocabulary the executor must not reason with. Registered strategy
	// types, as the rule table spells them.
	strategyTypes := []string{"single", "loadbalance", "conditional", "ab_split", "latency", "smart", "fallback"}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// Comments legitimately discuss strategies — the reasoning for a rule often
	// names the one that motivated it. Only code counts.
	comment := regexp.MustCompile(`(?m)^\s*//.*$`)

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		code := comment.ReplaceAllString(string(raw), "")

		for _, ty := range strategyTypes {
			if strings.Contains(code, `"`+ty+`"`) {
				t.Errorf("%s reasons about the strategy type %q.\n"+
					"The recovery engine reads a plan and an error class; how the plan was "+
					"produced is not its business. One special case here becomes a second, "+
					"and then the engine serves some strategies better than others — which "+
					"is the arrangement this rebuild replaced.", f, ty)
			}
		}
	}
}
