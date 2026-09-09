package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// The request-stage hook extraction runs through the traffic adapter that
// formatToTrafficAdapterID picks for the ingress format. A format that falls to
// the default arm gets `generic-jsonpath`, which does not know any vendor's
// request shape — so it yields no segments, the content hooks receive nothing,
// and the request is APPROVED with its content never scanned.
//
// That is not hypothetical. FormatOpenAIResponses was missing from the switch,
// so every /v1/responses request went upstream with a redact policy in force and
// its PII untouched: measured on prod, request decision APPROVE, and the stored
// request body carrying the value verbatim. The comment beside the switch called
// it "exhaustive" and the fallback "unreachable in practice".
//
// The formats are read out of the route registrations rather than listed here.
// A list written by hand cannot notice the format it forgot — which is exactly
// how this one survived.
func TestEveryRegisteredIngressFormatHasItsOwnTrafficAdapter(t *testing.T) {
	formats := ingressBodyFormatsFromRoutes(t)
	// Four ingress body formats are registered today. The floor is not a count to
	// keep updated — it is there so a parser that silently stops matching cannot
	// pass by covering nothing.
	if len(formats) < 4 {
		t.Fatalf("parsed only %d ingress BodyFormats out of the route file — the registration "+
			"shape changed, and a list that reads short passes by covering nothing", len(formats))
	}
	// FormatOpenAIResponses must be among them, and this is not decoration: it is
	// deliberately absent from AllFormats() (it has no standalone spec), so every
	// gate built by iterating AllFormats is blind to it. If the name resolution
	// below ever falls back to that enumeration, this line is what notices.
	var sawResponses bool
	for _, f := range formats {
		if f == provcore.FormatOpenAIResponses {
			sawResponses = true
		}
	}
	if !sawResponses {
		t.Fatalf("FormatOpenAIResponses is registered as an ingress body format but did not reach "+
			"this check — it is the format most likely to be invisible to a derived list, and "+
			"invisible is how it went unscanned. parsed: %v", formats)
	}

	var generic []string
	for _, f := range formats {
		if formatToTrafficAdapterID(f) == "generic-jsonpath" {
			generic = append(generic, string(f))
		}
	}
	sort.Strings(generic)

	if len(generic) > 0 {
		t.Errorf("these ingress formats fall to the generic-jsonpath traffic adapter:\n  %s\n\n"+
			"generic-jsonpath does not know their request shape, so the request-stage extraction "+
			"produces no segments, every content hook abstains, and the request is approved with "+
			"its content never scanned — while the audit row says APPROVE like any clean request. "+
			"Map each to the adapter that reads its wire.", strings.Join(generic, "\n  "))
	}
}

// ingressBodyFormatsFromRoutes reads `BodyFormat: provcore.FormatX` out of the
// route registrations with the parser, so the set comes from the routes rather
// than from a copy of them.
func ingressBodyFormatsFromRoutes(t *testing.T) []provcore.Format {
	t.Helper()
	path := filepath.Join("..", "..", "..", "cmd", "ai-gateway", "wiring", "routes.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	byName := provcoreFormatsByName()
	seen := map[provcore.Format]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "BodyFormat" {
			return true
		}
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if f, found := byName[sel.Sel.Name]; found {
			seen[f] = true
		}
		return true
	})

	out := make([]provcore.Format, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// provcoreFormatsByName maps every DECLARED Format constant name to its value.
//
// Read from the declarations, NOT from AllFormats(): that enumeration
// deliberately omits FormatOpenAIResponses, so building the name table from it
// would make this gate blind to exactly the format that went unscanned. A gate
// derived from an enumeration inherits that enumeration's blind spots.
func provcoreFormatsByName() map[string]provcore.Format {
	dir := filepath.Join("..", "..", "providers", "core")
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	out := map[string]provcore.Format{}
	for _, p := range entries {
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				return true
			}
			name := vs.Names[0].Name
			if !strings.HasPrefix(name, "Format") {
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
			out[name] = provcore.Format(val)
			return true
		})
	}
	return out
}
