package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// Every service must report ITS OWN identity, and the version must come from
// the build rather than from the source.
//
// This is the defect that made ResolveBuildIdentity necessary: three of the five
// services passed a hardcoded `"<service>/0.1.0"` and all five reported an empty
// buildSha, so no environment could be tied to a build and "is this fix
// deployed?" had to be answered by replaying traffic. Reshaping BuildInfo to
// take the two raw inputs removed the chance to compute the fields wrongly — it
// did not remove the chance to hand them the wrong VALUES, and nothing checked
// that. A sixth service added by copy-paste, or a service renamed with the
// string left behind, still lands two nodes under one name in the registry.
//
// Read with go/ast rather than grep: `Service:` and `BuildVersion:` are struct
// fields whose values are expressions, and the question here is what KIND of
// expression each is — a literal version string is the whole defect, and no
// text scan distinguishes a literal from an identifier that happens to look
// like one.
func TestBuildInfoCallSites_eachServiceNamesItselfAndTakesTheVersionFromTheBuild(t *testing.T) {
	type site struct {
		file    string
		line    int
		service string
		// versionIsLiteral is the defect: a version written into the source is
		// a version that stops being true the moment the source moves.
		versionIsLiteral bool
		versionLiteral   string
	}

	// The package sits at packages/shared/core/metrics/platform.
	repoRoot := filepath.Join("..", "..", "..", "..", "..")
	pkgDir := filepath.Join(repoRoot, "packages")
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("read packages dir: %v (looked at %s)", err, pkgDir)
	}

	var sites []site
	fset := token.NewFileSet()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cmdDir := filepath.Join(pkgDir, e.Name(), "cmd")
		if _, err := os.Stat(cmdDir); err != nil {
			continue
		}
		err := filepath.Walk(cmdDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || filepath.Ext(path) != ".go" {
				return err
			}
			if len(path) > 8 && path[len(path)-8:] == "_test.go" {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// A file this walk cannot parse is not this test's business:
				// the compiler and the linters already fail on it, and turning
				// a syntax error here into a BuildInfo verdict would report the
				// wrong subject. Skipped, and the walk continues.
				f = nil
			}
			if f == nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "BuildInfo" {
					return true
				}
				s := site{file: path, line: fset.Position(lit.Pos()).Line}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					switch key.Name {
					case "Service":
						if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
							s.service, _ = strconv.Unquote(bl.Value)
						}
					case "BuildVersion":
						if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
							s.versionIsLiteral = true
							s.versionLiteral, _ = strconv.Unquote(bl.Value)
						}
					}
				}
				sites = append(sites, s)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", cmdDir, err)
		}
	}

	// The walk has to be asserted before its findings are. A parser that
	// matched nothing reports a clean sweep, which is the failure mode this
	// whole file exists to close.
	const knownServices = 5 // hub, control-plane, ai-gateway, compliance-proxy, agent
	if len(sites) < knownServices {
		t.Fatalf("found %d BuildInfo call sites across packages/*/cmd, expected at least %d — "+
			"the walker has drifted and this test is asserting nothing", len(sites), knownServices)
	}

	byService := map[string][]site{}
	for _, s := range sites {
		if s.service == "" {
			t.Errorf("%s:%d passes a non-literal Service; the registry key a node appears under "+
				"must be readable here, not computed at runtime", s.file, s.line)
			continue
		}
		if s.versionIsLiteral {
			t.Errorf("%s:%d hardcodes BuildVersion = %q. A version written into the source stops "+
				"being true the moment the source moves — this is exactly what three of the five "+
				"services did with \"0.1.0\", leaving no way to tell which build a node ran. "+
				"Pass the -ldflags stamp instead.", s.file, s.line, s.versionLiteral)
		}
		byService[s.service] = append(byService[s.service], s)
	}

	// A service must not answer to a name that belongs to a different package:
	// two nodes reporting one name is indistinguishable, in the registry, from
	// one node restarting.
	for svc, ss := range byService {
		owners := map[string]bool{}
		for _, s := range ss {
			rel, err := filepath.Rel(pkgDir, s.file)
			if err != nil {
				continue
			}
			owners[firstSegment(rel)] = true
		}
		if len(owners) > 1 {
			var names []string
			for o := range owners {
				names = append(names, o)
			}
			sort.Strings(names)
			t.Errorf("service name %q is claimed from more than one package: %v — two nodes under "+
				"one name are indistinguishable in the registry from one node restarting",
				svc, names)
		}
	}

	if len(byService) < knownServices {
		t.Errorf("only %d distinct service names across %d call sites: %v — a copy-pasted "+
			"call site reporting a neighbour's name is the failure this catches",
			len(byService), len(sites), sortedKeys(byService))
	}
}

func firstSegment(rel string) string {
	for i := range len(rel) {
		if rel[i] == filepath.Separator {
			return rel[:i]
		}
	}
	return rel
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
