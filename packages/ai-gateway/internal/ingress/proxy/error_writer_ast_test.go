package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryGatewayErrorBodyGoesThroughTheIngressBuilder makes the claim in
// GatewayErrorBodyForIngress's doc comment enforceable instead of aspirational.
//
// That comment says every gateway error path goes through it "so no route can
// pick a different writer for the same code". When it was written that was not
// true, and the ways it was untrue were exactly the defects found in
// production: one code answering with two body shapes, and a refusal that
// swapped its own machine code on half the routes. A claim the code does not
// keep is how the next person reintroduces the same defect while believing the
// invariant holds.
//
// This is a go/ast walk rather than a grep because the property is structural —
// which function a call expression names — and a regex over source text reads
// comments, strings and test files as if they were calls. The standing lesson
// is that a structural question measured with a regex produces confident wrong
// answers.
//
// Allowed callers are listed with a reason. Adding one means deciding, on
// purpose, that a route may answer in a shape the shared builder would not
// choose — which is the decision this test exists to make deliberate.
func TestEveryGatewayErrorBodyGoesThroughTheIngressBuilder(t *testing.T) {
	// Direct GatewayErrorBody / GatewayErrorBodyWith calls that are NOT routed
	// through GatewayErrorBodyForIngress, with why each is legitimate.
	allowed := map[string]string{
		"openAIProxyErrorBody": "the thin wrapper the OpenAI-shaped paths read through; " +
			"it IS the OpenAI branch of the builder",
		"writeNoCompatibleCapability": "carries an available_capabilities array the ingress " +
			"builder has no slot for; embeddings ingress is OpenAI-shaped in every deployment",
		"writeResponsesFeatureRejection": "carries a param naming the offending field, and the " +
			"Responses API is the only ingress that reaches it",
	}

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "envelope" {
					return true
				}
				if sel.Sel.Name != "GatewayErrorBody" && sel.Sel.Name != "GatewayErrorBodyWith" {
					return true
				}
				if _, ok := allowed[fn.Name.Name]; ok {
					return true
				}
				offenders = append(offenders, fn.Name.Name+" ("+path+":"+
					fset.Position(call.Pos()).String()[strings.LastIndex(fset.Position(call.Pos()).String(), ":")+1:]+
					") calls envelope."+sel.Sel.Name)
				return true
			})
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("these build a gateway error body without asking which shape the caller's "+
			"ingress expects, so the same code can answer differently by route:\n  %s\n\n"+
			"Route it through envelope.GatewayErrorBodyForIngress, or add the function to "+
			"`allowed` above with the reason its shape is fixed.",
			strings.Join(offenders, "\n  "))
	}

	// A guard that cannot fail is not a guard. If someone deletes the last
	// direct call, the allowlist should shrink with it rather than sit here
	// implying a constraint that no longer binds anything.
	if len(allowed) == 0 {
		t.Fatal("the allowlist is empty; delete this test or the exemptions it documents")
	}
}
