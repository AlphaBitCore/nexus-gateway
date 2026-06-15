package assistant

// The web assistant runs in the cloud, where the model must never reach local
// host privilege (shell exec, host filesystem). That boundary is enforced by
// construction — BuildWebAgent omits the system capability group — but
// construction is easy to regress with a one-line option flip or a stray
// import. This test locks the boundary statically: it parses every
// non-test .go file in this package and fails if (a) it imports the
// local-execution packages (workflow-engine localtools/localhost, which carry
// bash/fs side effects) or (b) it constructs an agent registry with
// EnableSystem enabled. A failure here means someone is about to hand the
// cloud assistant a local privilege it must not have.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// bannedImportSubstrings are import paths the cloud assistant must never pull
// in — they are the local-host execution surface (shell + filesystem effects).
var bannedImportSubstrings = []string{
	"workflow-engine/localtools",
	"workflow-engine/localhost",
}

func assistantGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		t.Fatal("no non-test .go files found — test is looking in the wrong directory")
	}
	return files
}

// TestPrivilegeBoundary_NoLocalExecutionImports asserts the cloud assistant
// never imports the local bash/fs execution packages.
func TestPrivilegeBoundary_NoLocalExecutionImports(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range assistantGoFiles(t) {
		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, banned := range bannedImportSubstrings {
				if strings.Contains(path, banned) {
					t.Errorf("%s imports %q — the cloud assistant must never reach local execution (%s)", file, path, banned)
				}
			}
		}
	}
}

// TestPrivilegeBoundary_NeverEnablesSystem asserts no source in the package
// constructs AgentOptions with EnableSystem set true. It walks composite
// literals looking for an EnableSystem field whose value is the identifier
// `true`, so a future `runtime.AgentOptions{EnableSystem: true}` (or a bare
// `AgentOptions{..., EnableSystem: true}`) fails the build gate.
func TestPrivilegeBoundary_NeverEnablesSystem(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range assistantGoFiles(t) {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "EnableSystem" {
					continue
				}
				if val, ok := kv.Value.(*ast.Ident); ok && val.Name == "true" {
					pos := fset.Position(kv.Pos())
					t.Errorf("%s sets EnableSystem: true — the cloud assistant must never enable the local system capability group", pos)
				}
			}
			return true
		})
	}
}
