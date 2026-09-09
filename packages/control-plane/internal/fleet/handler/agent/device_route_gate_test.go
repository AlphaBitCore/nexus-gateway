package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// deviceScopedPathPrefixes are the route shapes whose authorization must be
// evaluated against the DEVICE, not the wildcard resource.
var deviceScopedPathPrefixes = []string{"/agent-devices/:id"}

// TestDeviceScopedRoutesUseTheDeviceAwareMiddleware refuses a per-device route
// registered under the plain IAM middleware.
//
// That combination fails OPEN, not closed, and silently: the plain middleware
// evaluates against the wildcard resource, and a policy statement scoped to
// `agent-device/group:<id>/*` never matches a wildcard target — so an
// administrator's group-scoped Deny is not merely outranked, it never enters
// the tally, and whatever unscoped Allow exists carries the request. Three
// routes (audit, config, timeline) shipped that way while their six siblings
// were correct.
//
// The scan is TREE-WIDE rather than package-local on purpose: the routes live in
// one package today, but a lint that watches only the file it was written for
// stops watching the moment the class moves. It is parsed with go/ast rather
// than grepped — a route registration is a call expression, and matching its
// third argument by text is how the next reader gets it subtly wrong.
func TestDeviceScopedRoutesUseTheDeviceAwareMiddleware(t *testing.T) {
	root := controlPlaneInternalDir(t)

	var scanned int
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Skipping was the original answer and it is the wrong one: a file
			// this gate cannot read is a file whose route registrations it
			// cannot check, reported as if there were none. The tree compiles,
			// so every production .go file parses — one that does not means
			// the gate has gone blind, and that is the finding.
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isEchoVerb(sel.Sel.Name) {
				return true
			}
			route, ok := stringLit(call.Args[0])
			if !ok || !isDeviceScoped(route) {
				return true
			}
			// Args[2:] are the middlewares. At least one must be the
			// device-aware factory.
			for _, arg := range call.Args[2:] {
				if mw, ok := arg.(*ast.CallExpr); ok {
					if id, ok := mw.Fun.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "device") {
						return true
					}
				}
			}
			pos := fset.Position(call.Pos())
			offenders = append(offenders, pos.Filename+":"+strconv.Itoa(pos.Line)+" "+route)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// A gate that reads nothing passes everything. Refuse to be vacuous.
	if scanned < 50 {
		t.Fatalf("only %d files scanned under %s — the gate is not looking at the tree it claims to police", scanned, root)
	}
	if len(offenders) > 0 {
		t.Fatalf("device-scoped routes registered under the plain IAM middleware (fails OPEN — a group-scoped Deny never enters the tally):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func isDeviceScoped(route string) bool {
	for _, p := range deviceScopedPathPrefixes {
		if strings.HasPrefix(route, p) {
			return true
		}
	}
	return false
}

func isEchoVerb(name string) bool {
	switch name {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// controlPlaneInternalDir walks up from this package to control-plane/internal.
func controlPlaneInternalDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 10 {
		if filepath.Base(dir) == "internal" && filepath.Base(filepath.Dir(dir)) == "control-plane" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate control-plane/internal from the test's working directory")
	return ""
}
