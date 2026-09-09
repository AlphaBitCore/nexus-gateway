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

// Extracting the window sizing into gatewayTailWindow makes the soundness test
// meaningful, but only about the FUNCTION. Nothing in a unit test observes what
// the relay actually hands the engine, so reverting the call site to the fixed
// constant — the original defect, verbatim — left every window test green.
//
// This closes that by asserting the call site structurally: within this package,
// every modela.Run must take its TailWindowBytes from gatewayTailWindow. It is
// an AST walk rather than a grep because the question is "what expression is
// this field initialised with", which text matching answers wrongly for a
// comment, a string, or a second Config built elsewhere.
func TestModelARunTakesItsWindowFromTheSizingFunction(t *testing.T) {
	fset := token.NewFileSet()
	found := 0

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		locals := localsAssignedFrom(f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Run" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "modela" {
				return true
			}
			found++
			pos := fset.Position(call.Pos())
			window, ok := tailWindowArgOf(call)
			if !ok {
				t.Errorf("%s: modela.Run does not set TailWindowBytes at all — the engine then "+
					"applies its own default, which is the fixed window this sizing exists to "+
					"replace", pos)
				return true
			}
			if !isCallTo(window, "gatewayTailWindow", locals) {
				t.Errorf("%s: modela.Run takes TailWindowBytes from something other than "+
					"gatewayTailWindow. The soundness gate asserts on that function, so a window "+
					"computed anywhere else is a window nothing checks — which is how this leg "+
					"came to run outside the engine's stated condition.", pos)
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("no modela.Run call found in this package; this gate has lost its subject")
	}
}

// tailWindowArgOf digs the TailWindowBytes initialiser out of a modela.Run call,
// whether the Config is written inline or bound to a local first.
func tailWindowArgOf(call *ast.CallExpr) (ast.Expr, bool) {
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "TailWindowBytes" {
				return kv.Value, true
			}
		}
	}
	return nil, false
}

// localsAssignedFrom maps each local assigned a direct function call to that
// function's name, so the check can follow `tailWindow := gatewayTailWindow(x)`
// through to `TailWindowBytes: tailWindow`. One level only: anything deeper is
// an indirection this gate cannot follow, and a rule that silently accepts what
// it cannot follow is the failure it replaces — so it reports the line instead.
func localsAssignedFrom(f *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if fn, ok := calleeName(as.Rhs[0]); ok {
			out[lhs.Name] = fn
		}
		return true
	})
	return out
}

// calleeName returns the name of the function a direct call invokes.
func calleeName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok {
		return "", false
	}
	return fn.Name, true
}

// isCallTo reports whether e is a call to the named function, or an identifier
// assigned from one.
func isCallTo(e ast.Expr, name string, locals map[string]string) bool {
	if fn, ok := calleeName(e); ok {
		return fn == name
	}
	if id, ok := e.(*ast.Ident); ok {
		return locals[id.Name] == name
	}
	return false
}
