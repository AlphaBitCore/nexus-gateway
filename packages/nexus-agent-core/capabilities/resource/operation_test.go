package resource

import (
	"strings"
	"testing"
)

func TestPathParams(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"/api/admin/virtual-keys", nil},
		{"/api/admin/virtual-keys/{id}", []string{"id"}},
		{"/api/admin/agents/{nodeId}/diagnostic-mode", []string{"nodeId"}},
		{"/api/admin/nodes/{id}/overrides/{configKey}", []string{"id", "configKey"}},
	}
	for _, c := range cases {
		got := pathParams(c.path)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("pathParams(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestSubstituteParams(t *testing.T) {
	// 0-param: unchanged.
	if got, err := SubstituteParams("/api/admin/cache/stats", nil); err != nil || got != "/api/admin/cache/stats" {
		t.Fatalf("0-param substitute = %q, %v", got, err)
	}
	// 1-param: filled + escaped.
	got, err := SubstituteParams("/api/admin/virtual-keys/{id}", map[string]string{"id": "vk/1"})
	if err != nil || got != "/api/admin/virtual-keys/vk%2F1" {
		t.Fatalf("1-param substitute = %q, %v", got, err)
	}
	// 2-param: BOTH filled (the subFirstParam predecessor left the 2nd brace behind).
	got, err = SubstituteParams("/api/admin/nodes/{id}/overrides/{configKey}", map[string]string{"id": "n1", "configKey": "cache.ttl"})
	if err != nil || got != "/api/admin/nodes/n1/overrides/cache.ttl" {
		t.Fatalf("2-param substitute = %q, %v", got, err)
	}
	// missing param is an error, not a half-substituted path.
	if _, err := SubstituteParams("/api/admin/nodes/{id}/overrides/{configKey}", map[string]string{"id": "n1"}); err == nil {
		t.Fatal("a missing 2nd param must error")
	} else if !strings.Contains(err.Error(), "configKey") {
		t.Fatalf("error must name the missing param, got %v", err)
	}
}

func TestCanonicalVerbAndLabel(t *testing.T) {
	mk := func(method, path string) Operation {
		return Operation{Kind: "virtual-keys", Method: method, Path: path, Params: pathParams(path)}
	}
	cases := []struct {
		op        Operation
		wantVerb  string
		wantLabel string
	}{
		{mk("GET", "/api/admin/virtual-keys"), "list", "list"},
		{mk("POST", "/api/admin/virtual-keys"), "create", "create"},
		{mk("GET", "/api/admin/virtual-keys/{id}"), "get", "get"},
		{mk("PUT", "/api/admin/virtual-keys/{id}"), "update", "update"},
		{mk("DELETE", "/api/admin/virtual-keys/{id}"), "delete", "delete"},
		{mk("POST", "/api/admin/virtual-keys/{id}/regenerate"), "action:regenerate", "action:regenerate"},
		// non-CRUD ops have no canonical verb but a tail label.
		{Operation{Kind: "semantic-cache", Method: "GET", Path: "/api/admin/semantic-cache/config"}, "", "config"},
		{Operation{Kind: "semantic-cache", Method: "PUT", Path: "/api/admin/semantic-cache/config"}, "", "put config"},
		{Operation{Kind: "jobs", Method: "GET", Path: "/api/admin/jobs/{id}/runs", Params: []string{"id"}}, "", "{id}/runs"},
	}
	for _, c := range cases {
		if v := c.op.CanonicalVerb(); v != c.wantVerb {
			t.Errorf("CanonicalVerb(%s %s) = %q, want %q", c.op.Method, c.op.Path, v, c.wantVerb)
		}
		if l := c.op.Label(); l != c.wantLabel {
			t.Errorf("Label(%s %s) = %q, want %q", c.op.Method, c.op.Path, l, c.wantLabel)
		}
	}
}

func TestMutating(t *testing.T) {
	if (Operation{Method: "GET"}).Mutating() {
		t.Error("GET must not be mutating")
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if !(Operation{Method: m}).Mutating() {
			t.Errorf("%s must be mutating", m)
		}
	}
}

func TestFindOp(t *testing.T) {
	op, ok := FindOp("virtual-keys", "setNodeOverride")
	if ok {
		t.Fatal("setNodeOverride is a nodes op, not virtual-keys")
	}
	op, ok = FindOp("nodes", "setNodeOverride")
	if !ok || op.Method != "PUT" || op.Path != "/api/admin/nodes/{id}/overrides/{configKey}" {
		t.Fatalf("FindOp(nodes,setNodeOverride) = %+v, %v", op, ok)
	}
	if len(op.Params) != 2 {
		t.Fatalf("setNodeOverride must have 2 path params, got %v", op.Params)
	}
	if _, ok := FindOp("no-such-kind", "x"); ok {
		t.Fatal("unknown kind must not resolve")
	}
	if _, ok := FindOp("nodes", "noSuchOp"); ok {
		t.Fatal("unknown operationId must not resolve")
	}
}

func TestSearchOperations(t *testing.T) {
	// A specific multi-word query surfaces the nodes override write near the top.
	got := Search("node override", 10)
	if len(got) == 0 {
		t.Fatal("search returned nothing for 'node override'")
	}
	var foundSet bool
	for _, op := range got {
		if op.OperationID == "setNodeOverride" {
			foundSet = true
		}
	}
	if !foundSet {
		t.Fatalf("search 'node override' must surface setNodeOverride, got %d results", len(got))
	}
	// An exact kind name ranks that kind's ops first.
	got = Search("virtual-keys", 5)
	if len(got) == 0 || got[0].Kind != "virtual-keys" {
		t.Fatalf("search 'virtual-keys' must rank that kind first, got %+v", got)
	}
	// limit is respected.
	if got := Search("a", 3); len(got) > 3 {
		t.Fatalf("limit not respected: got %d", len(got))
	}
	// A blank query browses the catalog (first N), never empty.
	if got := Search("", 7); len(got) != 7 {
		t.Fatalf("blank query must return the first 7, got %d", len(got))
	}
	// A query matching nothing returns nothing (not the whole catalog).
	if got := Search("zzzznotathing", 10); len(got) != 0 {
		t.Fatalf("non-matching query must return nothing, got %d", len(got))
	}
}

// TestEveryCatalogOperationReachable is the completeness guarantee the toolkit
// exists to prove: EVERY operation in the embedded OpenAPI catalog (all kinds, all
// methods, all nesting depths) resolves by operationId and substitutes to a
// concrete path with no leftover placeholder — i.e. nothing in the YAML is
// silently unreachable. A regression here means an endpoint dropped off the
// generic surface.
func TestEveryCatalogOperationReachable(t *testing.T) {
	total := 0
	var dupes []string
	for _, k := range resCatalog.Kinds {
		seen := map[string]bool{}
		for _, raw := range k.Operations {
			total++
			if raw.OperationID == "" {
				t.Errorf("%s has an operation with no operationId: %s %s", k.Kind, raw.Method, raw.Path)
				continue
			}
			if seen[raw.OperationID] {
				// A duplicate operationId (OpenAPI requires uniqueness) — the engine
				// resolves the first deterministically; the duplicate is shadowed. This
				// is a generator-side spec quirk surfaced here, not hidden.
				dupes = append(dupes, k.Kind+"/"+raw.OperationID+" ("+raw.Method+")")
				continue
			}
			seen[raw.OperationID] = true
			op, ok := FindOp(k.Kind, raw.OperationID)
			if !ok {
				t.Errorf("%s/%s is not resolvable by operationId", k.Kind, raw.OperationID)
				continue
			}
			params := map[string]string{}
			for _, p := range op.Params {
				params[p] = "x"
			}
			method, path, mutating, err := ResolveOperation(k.Kind, raw.OperationID, params)
			if err != nil {
				t.Errorf("%s/%s did not resolve: %v", k.Kind, raw.OperationID, err)
				continue
			}
			if strings.ContainsAny(path, "{}") {
				t.Errorf("%s/%s left a placeholder in the path: %s", k.Kind, raw.OperationID, path)
			}
			if !strings.EqualFold(method, raw.Method) {
				t.Errorf("%s/%s method = %s, want %s", k.Kind, raw.OperationID, method, raw.Method)
			}
			if mutating == strings.EqualFold(raw.Method, "GET") {
				t.Errorf("%s/%s mutating flag wrong for %s", k.Kind, raw.OperationID, raw.Method)
			}
		}
	}
	if total < 300 {
		t.Fatalf("catalog has only %d operations — expected the full ~364", total)
	}
	if len(dupes) > 0 {
		// operationId must be unique within a kind (each kind is one OpenAPI
		// document) so every operation is addressable by (kind, operationId). The
		// generator's assignOperationIDs enforces this.
		t.Fatalf("%d duplicate operationId(s) within a kind — every op must be uniquely addressable: %v", len(dupes), dupes)
	}
}

func TestResolveOperationUnknown(t *testing.T) {
	if _, _, _, err := ResolveOperation("nodes", "noSuchOp", nil); err == nil {
		t.Fatal("ResolveOperation must error on an unknown operationId")
	}
	// missing path param surfaces as an error (no half-path).
	if _, _, _, err := ResolveOperation("nodes", "setNodeOverride", map[string]string{"id": "n1"}); err == nil {
		t.Fatal("ResolveOperation must error on a missing path param")
	}
}

func TestCanonicalVerbsForKind(t *testing.T) {
	vk := resIdx["virtual-keys"]
	verbs := vk.canonicalVerbs()
	for _, want := range []string{"list", "get", "create", "update", "delete"} {
		if !contains(verbs, want) {
			t.Fatalf("virtual-keys canonical verbs missing %q: %v", want, verbs)
		}
	}
	// A singleton-config kind has few/no canonical verbs but still exposes operations.
	sc := resIdx["semantic-cache"]
	if len(sc.operations()) == 0 {
		t.Fatal("semantic-cache must expose operations")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestOpExtraCorpus asserts the search corpus carries the op summary + query-param
// names/descriptions, and excludes path-param names (a path param is structural,
// not a word an operator searches by).
func TestOpExtraCorpus(t *testing.T) {
	d := DistilledOp{
		Summary: "List blocked requests",
		Params: []DistilledParam{
			{Name: "provider", In: "query", Desc: "filter by provider id"},
			{Name: "nodeUuid", In: "path"},
		},
	}
	got := opExtraCorpus(d)
	for _, want := range []string{"list blocked requests", "provider", "filter by provider id"} {
		if !strings.Contains(got, want) {
			t.Fatalf("extra corpus missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "nodeuuid") {
		t.Fatalf("a path param must not enter the search corpus: %q", got)
	}
}

// TestScoreOperation_ExtraSurfacesByFilterWord proves the structural fix: a query
// that matches ONLY a query-param name (not the kind/operationId/path/label) scores
// zero without the extra corpus and non-zero with it — so "filter by provider" finds
// the op whose query has a `provider` param.
func TestScoreOperation_ExtraSurfacesByFilterWord(t *testing.T) {
	op := Operation{Kind: "traffic", Method: "GET", Path: "/api/admin/traffic", OperationID: "listTraffic"}
	terms := []string{"provider"}
	if s := scoreOperation(op, "", "provider", terms); s != 0 {
		t.Fatalf("'provider' must not match an op whose structural fields lack it, got %d", s)
	}
	if s := scoreOperation(op, "filter results by provider name", "provider", terms); s == 0 {
		t.Fatal("the extra corpus (a provider query param) should let 'provider' match the op")
	}
}

// TestOpIndexEnriched asserts the index is built at init and some ops carry enriched
// search text (proving the per-kind distill ran), so search is memoized + enriched.
func TestOpIndexEnriched(t *testing.T) {
	if len(opIndex) == 0 {
		t.Fatal("opIndex must be built at init")
	}
	enriched := 0
	for _, e := range opIndex {
		if e.extra != "" {
			enriched++
		}
	}
	if enriched == 0 {
		t.Fatal("opIndex should carry enriched search text (summary/query params) for some ops")
	}
}
