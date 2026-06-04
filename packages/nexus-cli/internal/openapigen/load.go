package openapigen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// loaded holds the type-checked control-plane packages plus an index from each
// resolved function/method object to its AST declaration, so the route walker
// can follow a handler reference (a *types.Func) to the body it must analyse.
type loaded struct {
	fset     *token.FileSet
	pkgs     []*packages.Package
	funcDecl map[*types.Func]*ast.FuncDecl
}

// loadControlPlane type-checks the packages matched by patterns under dir. The
// go.work workspace supplies dependencies, so response/request types defined in
// sibling packages resolve to full struct information.
func loadControlPlane(dir string, patterns, env []string) (*loaded, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports | packages.NeedTypesSizes,
		Dir:  dir,
		Fset: fset,
		Env:  env,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	var loadErr error
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			// Surface the first hard error; type-check errors make handler
			// analysis unreliable, so we fail loudly rather than emit a partial
			// spec that silently misses routes.
			if loadErr == nil {
				loadErr = fmt.Errorf("package %s: %s", p.PkgPath, e)
			}
		}
	})
	if loadErr != nil {
		return nil, loadErr
	}

	l := &loaded{fset: fset, pkgs: pkgs, funcDecl: map[*types.Func]*ast.FuncDecl{}}
	l.indexFuncDecls()
	return l, nil
}

// indexFuncDecls records, for every function and method declaration across all
// loaded (non-dependency) packages, the *types.Func object -> *ast.FuncDecl
// mapping used to resolve handler references.
func (l *loaded) indexFuncDecls() {
	for _, p := range l.pkgs {
		if p.TypesInfo == nil {
			continue
		}
		for _, file := range p.Syntax {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
					l.funcDecl[obj] = fd
				}
			}
		}
	}
}

// infoFor returns the type information for the package that owns expr's file.
// Route/handler analysis crosses package boundaries, so a single TypesInfo is
// not enough; the walker looks up the right one by the expression's position.
func (l *loaded) infoFor(pos token.Pos) *types.Info {
	for _, p := range l.pkgs {
		for _, f := range p.Syntax {
			if f.Pos() <= pos && pos <= f.End() {
				return p.TypesInfo
			}
		}
	}
	return nil
}

// render pretty-prints an AST expression back to source text (used to capture
// the IAM action expression verbatim for the x-nexus-iam-action extension).
func (l *loaded) render(expr ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, l.fset, expr); err != nil {
		return ""
	}
	return buf.String()
}
