package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every traffic row this package builds carries the caller's attribution tags.
//
// Six functions construct an audit.Record. One of them did the extraction
// inline and the other five did not, so X-Nexus-End-User-Id,
// X-Nexus-Session-Id and X-Nexus-Client-Tags worked on /v1/chat/completions and
// were silently dropped on realtime, transcription, guardrail, and both video
// arms. Nothing failed — the columns were simply null, which a customer
// discovers a quarter later when the per-user report does not add up.
//
// The check is over the syntax tree rather than over a list of handler names,
// because the failure mode is a handler added later: a name list would have to
// be remembered, and the thing nobody remembered is exactly how this happened.
// A function may satisfy it either by calling stampCallerAttribution or by
// setting the three fields in the literal itself — the realtime metering rows
// carry them across from the session record, which is the right seam there
// because that function has no request to read.
func TestEveryAuditRecordCarriesCallerAttribution(t *testing.T) {
	const helper = "stampCallerAttribution"
	fields := []string{"EndUserID", "SessionID", "ClientTags"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var lits []*ast.CompositeLit
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Record" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "audit" {
					lits = append(lits, lit)
				}
				return true
			})
			if len(lits) == 0 {
				continue
			}
			checked++

			callsHelper := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == helper {
					callsHelper = true
				}
				return true
			})
			if callsHelper {
				continue
			}
			for _, lit := range lits {
				set := map[string]bool{}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok {
						set[k.Name] = true
					}
				}
				var missing []string
				for _, f := range fields {
					if !set[f] {
						missing = append(missing, f)
					}
				}
				if len(missing) > 0 {
					t.Errorf("%s: %s builds an audit.Record without %s and never calls %s — "+
						"that endpoint's traffic rows carry no caller attribution",
						fset.Position(lit.Pos()), fn.Name.Name, strings.Join(missing, "/"), helper)
				}
			}
		}
	}

	// A check that found nothing to check is not a passing check.
	if checked < 5 {
		t.Fatalf("only %d record-building functions were found; the walk is not seeing the package", checked)
	}
}
