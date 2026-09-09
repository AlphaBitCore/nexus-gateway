package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The waist and the compliance layer must not branch on which provider a
// request came from.
//
// That is the whole point of having a waist: a hook reads the canonical spec and
// decides what to scan and what to redact in ONE shape, and each codec maps its
// own wire into that shape. The moment a compliance decision asks "is this
// Anthropic?", every future provider becomes a change in the compliance layer,
// and every provider already there becomes a case that can be forgotten.
//
// The rule holds today — an audit of the whole tree found provider names in
// these packages only in comments and examples, never in a condition. This test
// exists so it keeps holding, because the audit was manual and the next one
// would be too.
//
// Scope is deliberately these trees and not all of packages/shared. The
// normalize CODECS are per-provider by construction — an Anthropic codec that
// did not know it was Anthropic would be a strange thing — so the rule cannot
// and should not reach them. What it protects is the layer they all feed.
var providerBranchGuardedTrees = []string{
	filepath.Join("..", "..", "..", "transport", "normalize", "core"),
	filepath.Join("..", "..", "..", "policy", "hooks"),
	filepath.Join("..", "..", "..", "policy", "pipeline"),
	filepath.Join("..", "..", "..", "policy", "decision"),
}

// providerVocabulary reads the provider names from the declaration that defines
// them rather than from a list written here.
//
// A hand-written list guards against the names its author remembered. This one
// cannot go stale: a provider added to the Format constants is guarded from the
// same commit, and a provider renamed is guarded under its new name without
// anyone noticing this file exists.
func providerVocabulary(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "ai-gateway", "internal", "providers", "core", "format.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse the provider Format declarations at %s: %v", path, err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "Format") || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err == nil && v != "" {
				out[v] = name.Name
			}
		}
		return true
	})
	if len(out) < 10 {
		t.Fatalf("derived only %d provider names from %s — the declaration moved and this "+
			"test would now pass by knowing nothing", len(out), path)
	}
	return out
}

// providerBranchHits walks a tree and reports every branch condition that
// compares against a provider name.
//
// It reads the AST rather than the text because the question is structural. The
// same string appears in these packages dozens of times in comments naming an
// example wire, in a doc line, in a test fixture; a text scan reports all of
// them and the signal drowns. What matters is whether a name reaches a place
// that CHOOSES: an if condition, a switch tag, a case clause.
func providerBranchHits(t *testing.T, root string, vocab map[string]string) ([]string, int) {
	t.Helper()
	var hits []string
	scanned := 0
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		scanned++
		// note records a hit only for a string literal that IS a provider name.
		note := func(where string, expr ast.Node) {
			ast.Inspect(expr, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if constName, isProvider := vocab[v]; isProvider {
					pos := fset.Position(lit.Pos())
					hits = append(hits, filepath.ToSlash(path)+":"+strconv.Itoa(pos.Line)+
						" — "+where+" compares against "+strconv.Quote(v)+
						" (provcore."+constName+")")
				}
				return true
			})
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.IfStmt:
				if s.Cond != nil {
					note("an if condition", s.Cond)
				}
			case *ast.SwitchStmt:
				if s.Tag != nil {
					note("a switch tag", s.Tag)
				}
			case *ast.CaseClause:
				for _, e := range s.List {
					note("a case clause", e)
				}
			case *ast.ForStmt:
				if s.Cond != nil {
					note("a loop condition", s.Cond)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(hits)
	return hits, scanned
}

func TestWaistAndComplianceLayersDoNotBranchOnProviderIdentity(t *testing.T) {
	vocab := providerVocabulary(t)

	var all []string
	for _, tree := range providerBranchGuardedTrees {
		hits, scanned := providerBranchHits(t, tree, vocab)
		// A guarded tree that yielded no files is a path that moved, and a
		// clean report from it means nothing was looked at. That reads
		// identically to "the rule holds" in the output, which is the way this
		// kind of gate dies quietly.
		if scanned == 0 {
			t.Fatalf("%s produced no Go files to scan — the tree moved and this gate is "+
				"reporting clean on nothing", tree)
		}
		all = append(all, hits...)
	}
	if len(all) == 0 {
		return
	}
	t.Errorf("%d provider-identity branch(es) reached the waist / compliance layer.\n\n"+
		"These layers decide what to scan and what to redact, and they must do it on the "+
		"canonical spec alone — a condition naming a provider makes every future provider a "+
		"change here and every existing one a case that can be forgotten. Map the difference "+
		"in that provider's codec instead.\n\n  %s",
		len(all), strings.Join(all, "\n  "))
}
