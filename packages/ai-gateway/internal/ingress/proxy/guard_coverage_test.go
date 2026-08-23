package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// guard_coverage_test.go asserts the property that every defect in this class
// has violated: a guard applied on one selection path and not the other.
//
// There are exactly two paths to a dispatch target. The resolver
// (routing/resolver.go ResolveTargets) handles rule-driven routing. The
// explicit-model passthrough (resolveNoMatchPassthrough, here) handles "serve
// the model I named". They are reached by different requests and were written
// at different times, so a guard added to one silently missed the other:
//
//   - the RequiredModalities floor was resolver-only, so an explicit
//     gpt-audio-mini on a text chat passed every gateway check and 400'd
//     upstream;
//   - the embeddings capability pre-filter was resolver-only, so an explicit
//     embeddings model with an unsupported dimensions value did the same.
//
// Both are now on both paths. This is the gate that says so, and that fails
// when a third guard is added to one path only.
//
// It reads production source rather than driving requests because the claim is
// about which call sites exist, not about what one request does. A behavioural
// test proves the guard fires for the case it was written with; only the
// enumeration proves no path was skipped.
//
// The floor + input-modality guards now sit behind namedModelModalityGuard (the
// #297 EnforceNamedModelModality gate: a NAMED model's modality verdict is the
// upstream's by default). That is a change in WHEN they fire, not WHETHER the
// path reaches them — so this gate follows the wrapper transitively rather than
// weakening to "reachable somehow". The passthrough entry must invoke the
// wrapper + the embeddings guard directly; the wrapper must invoke the two
// modality guards. Drop a guard at either level and this fails.

// passthroughDirectGuards are invoked straight from the explicit-model entry.
var passthroughDirectGuards = []string{
	"namedModelModalityGuard",
	"embeddingCapabilityGuard",
}

// namedModalityGuards are the modality guards the wrapper must invoke — the
// floor and the input-modality ceiling, gated together by the #297 flag.
var namedModalityGuards = []string{
	"floorGuard",
	"inputModalityGuard",
}

// passthroughEntry is the function that resolves an explicitly-named model.
const passthroughEntry = "resolveNoMatchPassthrough"

// namedModalityWrapper is the flag-gated wrapper the entry delegates the
// modality floor+ceiling to.
const namedModalityWrapper = "namedModelModalityGuard"

func TestGuardCoverage_ExplicitModelPathInvokesEveryGuard(t *testing.T) {
	fromEntry := callsWithin(t, passthroughEntry)
	if len(fromEntry) == 0 {
		t.Fatal("found no calls at all inside " + passthroughEntry +
			" — the parser has drifted from the code it guards, which would let this gate pass while guarding nothing")
	}
	for _, guard := range passthroughDirectGuards {
		if !fromEntry[guard] {
			t.Errorf("%s does not invoke %s.\n"+
				"The resolver applies it; a request that names its model directly would skip it. "+
				"Every defect in this class is a guard that held on one selection path and not the other.",
				passthroughEntry, guard)
		}
	}

	fromWrapper := callsWithin(t, namedModalityWrapper)
	if len(fromWrapper) == 0 {
		t.Fatal("found no calls inside " + namedModalityWrapper +
			" — the modality floor/ceiling wrapper has drifted, which would let the entry claim coverage it does not have")
	}
	for _, guard := range namedModalityGuards {
		if !fromWrapper[guard] {
			t.Errorf("%s does not invoke %s.\n"+
				"The floor and input-modality guards are gated together here; dropping one lets a named "+
				"request skip it even when EnforceNamedModelModality is on.",
				namedModalityWrapper, guard)
		}
	}
}

// The guards must be reachable unconditionally from the entry, not buried
// behind a branch that a future refactor could narrow — the modality guard
// above them is a plain statement for the same reason.
func TestGuardCoverage_GuardsAreDeclared(t *testing.T) {
	declared := map[string]bool{}
	forEachFuncDecl(t, func(fn *ast.FuncDecl) {
		declared[fn.Name.Name] = true
	})
	guards := append(append([]string{}, passthroughDirectGuards...), namedModalityGuards...)
	for _, guard := range guards {
		if !declared[guard] {
			t.Errorf("%s is listed as a guard but no such function exists — the list has drifted from the code", guard)
		}
	}
	if !declared[passthroughEntry] {
		t.Fatalf("%s no longer exists; this gate is checking a function that is gone", passthroughEntry)
	}
	if !declared[namedModalityWrapper] {
		t.Fatalf("%s no longer exists; the modality floor/ceiling gate is checking a function that is gone", namedModalityWrapper)
	}
}

func callsWithin(t *testing.T, fnName string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	forEachFuncDecl(t, func(fn *ast.FuncDecl) {
		if fn.Name.Name != fnName || fn.Body == nil {
			return
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				out[f.Name] = true
			case *ast.SelectorExpr:
				out[f.Sel.Name] = true
			}
			return true
		})
	})
	return out
}

func forEachFuncDecl(t *testing.T, visit func(*ast.FuncDecl)) {
	t.Helper()
	for _, file := range parseProductionSources(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				visit(fn)
			}
			return true
		})
	}
}

// parseProductionSources reads this directory's non-test Go files.
//
// Reading the directory ourselves rather than through parser.ParseDir says what
// this gate actually wants — these files, in this directory — instead of asking
// for "the packages here" and then discarding the grouping. It also fails loudly
// on an empty result: a gate that parsed nothing would report no violations and
// look identical to a gate that found none.
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
		t.Fatal("parsed no production sources; this gate would pass while guarding nothing")
	}
	return files
}
