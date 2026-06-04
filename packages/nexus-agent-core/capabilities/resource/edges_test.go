package resource

import (
	"strings"
	"testing"
)

// TestErrUnknownOperationMessage asserts ResolveOperation's unknown-op error names
// both the kind and the operationId, so a caller (cli/tui) can show what failed.
func TestErrUnknownOperationMessage(t *testing.T) {
	_, _, _, err := ResolveOperation("nodes", "noSuchOp", nil)
	if err == nil {
		t.Fatal("ResolveOperation must error on an unknown op")
	}
	msg := err.Error()
	if !strings.Contains(msg, "noSuchOp") || !strings.Contains(msg, "nodes") {
		t.Fatalf("error must name the kind + op, got %q", msg)
	}
}

// TestEnumStrings renders OpenAPI enums (incl. non-string 3.1 members) to display
// strings; an empty enum yields nil so a field renders as free input.
func TestEnumStrings(t *testing.T) {
	got := enumStrings([]any{"keep", 2, true})
	if len(got) != 3 || got[0] != "keep" || got[1] != "2" || got[2] != "true" {
		t.Fatalf("enumStrings mixed members = %v", got)
	}
	if enumStrings(nil) != nil {
		t.Fatal("an empty enum must yield nil")
	}
}

// TestDescribeOperationCarriesParams asserts DescribeOperation surfaces an op's
// input parameters (the cascade builds filter/path inputs from them). At least one
// catalog op must declare parameters; the FieldInfo carries name + in.
func TestDescribeOperationCarriesParams(t *testing.T) {
	for _, k := range Kinds() {
		for _, op := range Operations(k.Kind) {
			s, ok := DescribeOperation(k.Kind, op.OperationID)
			if !ok || len(s.Params) == 0 {
				continue
			}
			p := s.Params[0]
			if p.Name == "" || p.In == "" {
				t.Fatalf("%s/%s param missing name/in: %+v", k.Kind, op.OperationID, p)
			}
			return // found a params-bearing op and asserted it
		}
	}
	t.Fatal("expected at least one catalog operation to declare parameters")
}

// TestDistillKindMalformedYAML: a kind whose spec bytes are invalid YAML returns an
// error rather than panicking (an external/edited spec must fail safely).
func TestDistillKindMalformedYAML(t *testing.T) {
	rk := resourceKind{Kind: "x", Operations: []resourceOp{{Method: "GET", Path: "/api/admin/x", OperationID: "listX"}}}
	if _, err := distillKind(rk, []byte("paths: [unterminated")); err == nil {
		t.Fatal("malformed spec YAML must surface an error")
	}
}

// TestDistillBodyIgnoresNonJSON: a request body declared only as a non-JSON media
// type contributes no distilled body fields (we describe the JSON contract only).
func TestDistillBodyIgnoresNonJSON(t *testing.T) {
	rk := resourceKind{Kind: "x", Operations: []resourceOp{{Method: "POST", Path: "/api/admin/x", OperationID: "createX"}}}
	raw := []byte("paths:\n" +
		"  /api/admin/x:\n" +
		"    post:\n" +
		"      requestBody:\n" +
		"        content:\n" +
		"          text/plain:\n" +
		"            schema:\n" +
		"              type: string\n")
	d, err := distillKind(rk, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Operations) != 1 || len(d.Operations[0].Body) != 0 {
		t.Fatalf("a non-JSON request body must distill no body fields, got %+v", d.Operations)
	}
}

// TestCanonicalVerbNonCanonicalShapes: a method that does not map to a CRUD shape on
// the collection or the /{id} item yields no canonical verb (the op is still
// reachable, it just falls back to a tail label).
func TestCanonicalVerbNonCanonicalShapes(t *testing.T) {
	coll := collectionPath("x") // /api/admin/x
	cases := []Operation{
		{Kind: "x", Method: "DELETE", Path: coll},         // DELETE on a collection
		{Kind: "x", Method: "POST", Path: coll + "/{id}"}, // POST directly on an item
		{Kind: "x", Method: "HEAD", Path: coll + "/{id}"}, // an unmapped item method
	}
	for _, op := range cases {
		if v := op.CanonicalVerb(); v != "" {
			t.Fatalf("%s %s must have no canonical verb, got %q", op.Method, op.Path, v)
		}
	}
}

// TestLabelFallsBackToMethod: an operation whose path is under neither the kind's
// collection nor the base prefix labels by its (lower-cased) method, never blank.
func TestLabelFallsBackToMethod(t *testing.T) {
	op := Operation{Kind: "x", Method: "GET", Path: "/weird/place"}
	if l := op.Label(); l != "get" {
		t.Fatalf("a path outside the base prefix must label by method, got %q", l)
	}
}

// TestUnbalancedBraceHandledGracefully: an unterminated "{" in a path neither panics
// nor errors — pathParams skips it and SubstituteParams emits it verbatim.
func TestUnbalancedBraceHandledGracefully(t *testing.T) {
	if p := pathParams("/api/admin/x/{id"); len(p) != 0 {
		t.Fatalf("an unbalanced brace yields no params, got %v", p)
	}
	got, err := SubstituteParams("/api/admin/x/{id", nil)
	if err != nil || got != "/api/admin/x/{id" {
		t.Fatalf("an unbalanced brace must pass through verbatim, got %q err=%v", got, err)
	}
}

// TestSearchIndexesDescription proves the corpus indexes an operation's DESCRIPTION,
// not just its summary: "pipeline" surfaces hookExecutionChain (summary "Get hook
// execution chain", description mentions the pipeline visualiser) — previously that
// op was invisible to the word "pipeline" and only listHookConfigs matched.
func TestSearchIndexesDescription(t *testing.T) {
	var sawChain bool
	for _, op := range Search("pipeline", 8) {
		if op.OperationID == "hookExecutionChain" {
			sawChain = true
		}
	}
	if !sawChain {
		t.Fatal("'pipeline' must surface hookExecutionChain via its description text")
	}
}

// TestSearchLimitDefaultAndTokenMatch: a non-positive limit defaults to 20, and a
// partial token contained in a kind name still surfaces that kind.
func TestSearchLimitDefaultAndTokenMatch(t *testing.T) {
	if got := Search("a", 0); len(got) > 20 {
		t.Fatalf("a non-positive limit must default to 20, got %d", len(got))
	}
	var sawRouting bool
	for _, op := range Search("rout", 5) {
		if op.Kind == "routing-rules" {
			sawRouting = true
		}
	}
	if !sawRouting {
		t.Fatal("the token 'rout' should surface the routing-rules kind")
	}
}
