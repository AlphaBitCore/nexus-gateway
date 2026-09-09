package httpclient_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every outbound *http.Client must come from this package's New or NewProbe.
// forbidigo cannot express that rule. It matches a rendered SELECTOR, so the
// only pattern it can hold is `http.Client` — which fires on every parameter,
// struct field and interface method naming the type, because you cannot hold a
// client without naming its type. Constructing one is a composite literal, and
// only the AST tells the two apart.
//
// Parsing, not type-checking: go/parser reads a file standalone, so this reaches
// every module without caring which ones go.work lists — and two of them are
// deliberately outside it.
//
// One axis where a parser is WEAKER than a type-aware linter: forbidigo resolves
// an import to its real package name and so sees through an alias or a
// dot-import. A parser cannot, so the local name is recovered from each file's
// import block instead and every binding is matched.

// allowed lists the files that may build a client directly, each with a reason.
// There is exactly one, and the entry below is not a grandfathered backlog: every
// other caller was moved onto the factory rather than excused, which is what made
// the list collapse from twelve to one. A stale entry fails too, so this cannot
// quietly refill.
var allowed = map[string]string{
	"packages/httpclient/httpclient.go": "the factory itself",
}

// scanRoots are the trees carrying Go this rule governs. tools/ is not among
// them: it holds no Go at all, and naming it would claim a breadth that does not
// exist.
var scanRoots = []string{"packages/", "tests/"}

// minFiles guards the shape this gate is most likely to fail in — reaching part
// of the tree and reporting clean. packages/shared alone is around 490 files, so
// a floor of 500 would pass a walk that saw one module.
const minFiles = 1800

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.work or .git above the working directory; this gate cannot " +
				"locate the repo, and a pass would mean nothing")
		}
		dir = parent
	}
}

// httpNames returns the local names net/http is bound to in this file. Usually
// just "http"; an alias or a dot-import changes it, and a parser has no other
// way to know. An empty string means the dot-import case, where the selector
// disappears entirely.
func httpNames(f *ast.File) []string {
	var names []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "net/http" {
			continue
		}
		switch {
		case imp.Name == nil:
			names = append(names, "http")
		case imp.Name.Name == ".":
			names = append(names, "")
		case imp.Name.Name == "_":
			// A blank import cannot construct anything.
		default:
			names = append(names, imp.Name.Name)
		}
	}
	return names
}

// clientExprPos reports the position of a client CONSTRUCTION at n, or 0. Three
// spellings build one without New: a composite literal, new(), and a var
// declaration of the type.
func clientExprPos(n ast.Node, names []string) token.Pos {
	isClientType := func(e ast.Expr) bool {
		switch t := e.(type) {
		case *ast.SelectorExpr:
			if t.Sel.Name != "Client" {
				return false
			}
			pkg, ok := t.X.(*ast.Ident)
			if !ok {
				return false
			}
			for _, name := range names {
				if name != "" && pkg.Name == name {
					return true
				}
			}
		case *ast.Ident:
			// Only reachable through a dot-import, where "" is in names.
			if t.Name != "Client" {
				return false
			}
			for _, name := range names {
				if name == "" {
					return true
				}
			}
		}
		return false
	}

	switch v := n.(type) {
	case *ast.CompositeLit:
		if isClientType(v.Type) {
			return v.Pos()
		}
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 {
			if isClientType(v.Args[0]) {
				return v.Pos()
			}
		}
	case *ast.ValueSpec:
		if v.Type != nil && isClientType(v.Type) {
			return v.Pos()
		}
	}
	return 0
}

func TestNoBareHTTPClientConstruction(t *testing.T) {
	root := repoRoot(t)

	var offenders []string
	scanned := 0
	hits := map[string]int{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", "_archive":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		// Tests build throwaway clients for fixtures, matching the _test.go
		// waiver the lint config already grants. Helper PACKAGES under tests/
		// are not test files and stay in scope.
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		inScope := false
		for _, prefix := range scanRoots {
			if strings.HasPrefix(rel, prefix) {
				inScope = true
				break
			}
		}
		if !inScope {
			return nil
		}

		// A file this gate cannot parse is not a pass, and not a reason to
		// abandon the walk either: one bad file must not hide the tree. The
		// failure becomes a message so it can be recorded and skipped.
		fset := token.NewFileSet()
		f, unparseable := parseOrReason(fset, path)
		if unparseable != "" {
			offenders = append(offenders, rel+": unparseable — "+unparseable)
			return nil
		}
		scanned++

		names := httpNames(f)
		if len(names) == 0 {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			pos := clientExprPos(n, names)
			if pos == 0 {
				return true
			}
			if _, ok := allowed[rel]; ok {
				hits[rel]++
				return true
			}
			offenders = append(offenders, rel+":"+strconv.Itoa(fset.Position(pos).Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A gate that scanned part of the tree and a gate that found nothing produce
	// identical output.
	if scanned < minFiles {
		t.Fatalf("only %d Go files parsed under %s, expected at least %d — the walk is "+
			"not reaching the tree, so a clean result here would mean nothing",
			scanned, root, minFiles)
	}

	// Per entry, not one counter: a single live entry would otherwise mask any
	// number of dead ones, and the backlog this list exists to keep visible
	// would rot in place.
	var stale []string
	for path := range allowed {
		if hits[path] == 0 {
			stale = append(stale, path)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("allowlist entries that matched no construction:\n  %s\n\n"+
			"Either the file moved, the construction was removed — delete the entry — "+
			"or the AST match has broken, in which case this gate is no longer looking "+
			"at what it names.", strings.Join(stale, "\n  "))
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("bare http.Client construction outside the allowlist:\n  %s\n\n"+
			"Use packages/httpclient.New (or NewProbe). If this call genuinely "+
			"cannot — it needs Timeout=0, system roots before config, or a transport it "+
			"swaps at runtime — add it to `allowed` in this file WITH the reason.",
			strings.Join(offenders, "\n  "))
	}
}

// parseOrReason parses one file, returning the failure as text so the caller can
// record it and carry on. Returning an error here would end the walk.
func parseOrReason(fset *token.FileSet, path string) (*ast.File, string) {
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err.Error()
	}
	return f, ""
}
