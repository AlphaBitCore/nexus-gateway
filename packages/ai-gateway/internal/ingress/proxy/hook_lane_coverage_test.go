package proxy

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

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Every wire this gateway serves has to end up somewhere that scans it, and
// there are four somewheres:
//
//   - the canonical waist, where a hook sees the structure the codecs speak;
//   - the gateway-local multimodal extraction, for JSON wires whose user text is
//     a named field rather than a conversation;
//   - the format-aware flat extraction, sound only where the content genuinely
//     IS a flat list of strings;
//   - a dedicated handler with its own scan, for the wires that never ride
//     ServeProxy at all (multipart).
//
// The danger is not the wires on each lane today — it is the next one added. A
// new wire joins the flat lane by doing nothing at all, and a wire routed to its
// own handler is scanned only if somebody remembered to make it scan. Both
// failures are silent: an unscanned request looks exactly like a clean one.
//
// So the vocabulary is READ rather than restated. typology's WireShape constants
// come out of its source with the parser and are classified by the same
// predicates the code uses; every acknowledgement has to name something the
// parser can then confirm still exists.
func TestEveryWireShapeIsScannedSomewhere(t *testing.T) {
	shapes := wireShapesFromSource(t)
	if len(shapes) < 20 {
		t.Fatalf("parsed only %d wire shapes out of typology's source — the const block moved or "+
			"changed form, and a vocabulary that reads short passes this test by covering nothing",
			len(shapes))
	}

	// Kinds acknowledged on the FLAT lane. The flat model is not a lossy
	// projection for these: the content genuinely is a list of strings, and both
	// directions are gated per wire in
	// shared/traffic/adapters/embeddings_scan_coverage_test.go.
	flatIsTheShape := map[typology.EndpointKind]string{
		typology.EndpointKindEmbeddings: "input[] / texts[] / content.parts[].text — a flat list of documents",
		typology.EndpointKindRerank:     "{query, documents[]} — a query plus each document, also flat",
	}

	// Kinds served by their own handler, which never reaches stage_hooks. Each
	// entry names the function that scans that wire, and the parser confirms the
	// function is still there — an acknowledgement whose subject was deleted is
	// how a gap gets re-opened under a green test.
	ownHandlerScan := map[typology.EndpointKind]string{
		typology.EndpointKindVideoGeneration: "scanVideoPrompt",
		typology.EndpointKindSTT:             "scanSTTPrompt",
	}

	// Kinds with no route registered. Verified below against the routes file, so
	// mounting one turns this red rather than letting it arrive unscanned.
	notMounted := map[typology.EndpointKind]string{
		typology.EndpointKindBatch: "/v1/batches",
	}

	funcs := funcNamesInPackage(t, ".")
	for kind, fn := range ownHandlerScan {
		if !funcs[fn] {
			t.Errorf("%q is acknowledged as scanned by its own handler's %s, and no such function "+
				"exists any more — the wire is either unscanned or scanned by something this list "+
				"cannot see", kind, fn)
		}
	}
	routes := readFileOrSkip(t, filepath.Join("..", "..", "..", "cmd", "ai-gateway", "wiring", "routes.go"))
	for kind, path := range notMounted {
		if strings.Contains(routes, `"POST `+path+`"`) {
			t.Errorf("%q is acknowledged as not mounted, but %s is registered now — decide which "+
				"lane scans it before it serves traffic", kind, path)
		}
	}

	unclassified := map[typology.EndpointKind][]string{}
	for name, shape := range shapes {
		kind := typology.KindFromWireShape(shape)
		switch {
		case kind == "":
			// The body-less sentinel: catalog reads carry no content.
		case kind == typology.EndpointKindChat:
			// The canonical waist.
		case multimodalUserTextPaths(kind) != nil:
			// The gateway-local multimodal extraction.
		case flatIsTheShape[kind] != "":
		case ownHandlerScan[kind] != "":
		case notMounted[kind] != "":
		default:
			unclassified[kind] = append(unclassified[kind], name)
		}
	}

	var lines []string
	for kind, names := range unclassified {
		sort.Strings(names)
		lines = append(lines, string(kind)+" (from "+strings.Join(names, ", ")+")")
	}
	sort.Strings(lines)
	if len(lines) > 0 {
		t.Errorf("these endpoint kinds reach the FLAT hook extraction without being acknowledged "+
			"for it:\n  %s\n\nA kind lands there by doing nothing, so this is what silence looks "+
			"like. Decide which it is: give the wire a canonical lane if it has structure a flat "+
			"list of strings cannot hold (a tool call, a reasoning turn, a tool result); give it "+
			"the gateway-local multimodal extraction if its user text is a named field; or add it "+
			"above with the reason the flat model IS its shape — and gate the extraction AND the "+
			"rewrite per wire, the way embeddings and rerank are.",
			strings.Join(lines, "\n  "))
	}

	// Anti-vacuity: every acknowledgement must name a kind some wire actually
	// maps to, or the lists silently become a record of the past.
	reachable := map[typology.EndpointKind]bool{}
	for _, shape := range shapes {
		reachable[typology.KindFromWireShape(shape)] = true
	}
	for _, set := range []map[typology.EndpointKind]string{flatIsTheShape, ownHandlerScan, notMounted} {
		for kind := range set {
			if !reachable[kind] {
				t.Errorf("%q is acknowledged but no wire shape maps to it — the entry is stale, or "+
					"the mapping changed and this test now guards an empty set", kind)
			}
		}
	}
}

// wireShapesFromSource reads typology's WireShape constants with the parser, so
// the vocabulary comes from the declaration rather than from a copy of it here.
func wireShapesFromSource(t *testing.T) map[string]typology.WireShape {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "shared", "transport", "typology", "wireshape.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	out := map[string]typology.WireShape{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			return true
		}
		name := vs.Names[0].Name
		if !strings.HasPrefix(name, "WireShape") {
			return true
		}
		lit, ok := vs.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, uerr := strconv.Unquote(lit.Value)
		if uerr != nil {
			return true
		}
		out[name] = typology.WireShape(val)
		return true
	})
	return out
}

// funcNamesInPackage collects every function and method name declared in a
// directory, so an acknowledgement can name one and be checked against it.
//
// The files are walked directly rather than through parser.ParseDir, which is
// deprecated for not honouring build tags. Tags do not matter for a name census
// — a name behind any tag is still a name this package can be said to have — and
// walking the directory keeps the dependency at stdlib.
func funcNamesInPackage(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				out[fn.Name.Name] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed no functions out of %s — the check below would accept any name", dir)
	}
	return out
}

func readFileOrSkip(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — the not-mounted acknowledgements cannot be verified without it", path, err)
	}
	return string(b)
}
