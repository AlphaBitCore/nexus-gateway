package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// egress_paths_test.go is the structural half of the guarantee that
// egress_capture_test.go asserts behaviourally.
//
// That file proves the internal carriers do not reach the transport on the two
// legs it drives. It cannot prove anything about a leg that does not exist yet,
// and that is exactly how the defect recurred: the strip lived in the codecs
// until Cohere forwarded the namespace and 422'd, then moved to PrepareBody and
// was called one dispatcher guarantee — until the cross-format failover leg,
// which builds its body in canonicalbridge and calls ExecuteWithBody directly,
// turned out never to touch PrepareBody. Each fix was correct for the paths
// anyone had thought to enumerate.
//
// The fix that finally held put the strip at executeWithBodyAndURL, on the
// claim that every dispatch path funnels through it. This file makes that claim
// checkable instead of remembered: it reads the dispatcher's own source and
// fails if any method sends a request without going through that funnel.

// requestSendingCalls are the transport operations that actually put a caller's
// request on the wire. Probe and ListModels are deliberately absent — they are
// admin-path operations that construct their own bodies and carry no caller
// content, so the carrier guarantee has nothing to say about them.
var requestSendingCalls = []string{"Transport.Do"}

// terminalEgressFrame is the single method every request-sending path must
// funnel through. It is where the internal-carrier strip lives.
const terminalEgressFrame = "executeWithBodyAndURL"

func TestEgressPaths_OnlyTheTerminalFrameSendsRequests(t *testing.T) {
	senders := map[string]bool{}
	for _, file := range parseProductionSources(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if matchesRequestSend(call.Fun) {
					senders[fn.Name.Name] = true
				}
				return true
			})
			return true
		})
	}

	if len(senders) == 0 {
		t.Fatal("found no request-sending call at all — the matcher has drifted from the code it guards, " +
			"which would let this gate pass while guarding nothing")
	}

	var names []string
	for n := range senders {
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) != 1 || names[0] != terminalEgressFrame {
		t.Errorf("request-sending methods = %v, want exactly [%s].\n"+
			"A path that reaches the transport without passing through %s does not get the "+
			"internal-carrier strip, and the last two times that happened the namespace went "+
			"upstream and the provider answered 422. Either route the new path through the "+
			"terminal frame, or move the guarantee to wherever they now converge.",
			names, terminalEgressFrame, terminalEgressFrame)
	}
}

// The strip has to be IN the terminal frame, not merely reachable from it. A
// refactor that moves it into a helper called from one branch would leave the
// structural check above green while the guarantee quietly narrows.
func TestEgressPaths_TerminalFrameStripsCarriers(t *testing.T) {
	var found bool
	for _, file := range parseProductionSources(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != terminalEgressFrame || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "stripInternalCarriers" {
						found = true
					}
				}
				return true
			})
			return true
		})
	}
	if !found {
		t.Errorf("%s does not call stripInternalCarriers directly; the carrier guarantee is no longer "+
			"stated at the frame every path funnels through", terminalEgressFrame)
	}
}

// parseProductionSources reads this directory's non-test Go files — a gate that
// read the test files would be checking its own fixtures.
//
// Reading the directory ourselves says what these gates actually want: these
// files, in this directory. It also fails loudly on an empty result, because a
// gate that parsed nothing reports no violations and looks exactly like a gate
// that found none.
func parseProductionSources(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no production sources; these gates would pass while guarding nothing")
	}
	return files
}

// matchesRequestSend reports a selector chain ending in one of the
// request-sending transport calls, e.g. a.spec.Transport.Do(...).
func matchesRequestSend(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	chain := inner.Sel.Name + "." + sel.Sel.Name
	for _, want := range requestSendingCalls {
		if chain == want {
			return true
		}
	}
	return false
}
