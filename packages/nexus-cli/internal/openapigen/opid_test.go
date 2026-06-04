package openapigen

import "testing"

func opID(routes []route, method, path string) string {
	for _, r := range routes {
		if r.Method == method && r.Path == path {
			return r.OperationID
		}
	}
	return ""
}

func TestAssignOperationIDs(t *testing.T) {
	routes := []route{
		// One handler bound to PUT+PATCH on the same path → disambiguate by method.
		{Method: "PUT", Path: "/api/admin/routing-rules/:id", handlerName: "UpdateRoutingRule"},
		{Method: "PATCH", Path: "/api/admin/routing-rules/:id", handlerName: "UpdateRoutingRule"},
		// One handler bound to two sibling POSTs → disambiguate by trailing segment;
		// the member whose tail matches the handler name keeps the bare id.
		{Method: "POST", Path: "/api/admin/hooks/:id/test", handlerName: "HookTest"},
		{Method: "POST", Path: "/api/admin/hooks/:id/dry-run", handlerName: "HookTest"},
		// Same base id in a DIFFERENT kind must NOT be disambiguated (separate doc).
		{Method: "GET", Path: "/api/admin/extract-cache/config", handlerName: "GetConfig"},
		{Method: "GET", Path: "/api/admin/semantic-cache/config", handlerName: "GetConfig"},
		// A unique base keeps it.
		{Method: "GET", Path: "/api/admin/virtual-keys", handlerName: "ListVirtualKeys"},
	}
	assignOperationIDs(routes, "/api/admin")

	want := map[[2]string]string{
		{"PUT", "/api/admin/routing-rules/:id"}:     "updateRoutingRulePut",
		{"PATCH", "/api/admin/routing-rules/:id"}:   "updateRoutingRulePatch",
		{"POST", "/api/admin/hooks/:id/test"}:       "hookTest",
		{"POST", "/api/admin/hooks/:id/dry-run"}:    "hookTestDryRun",
		{"GET", "/api/admin/extract-cache/config"}:  "getConfig",
		{"GET", "/api/admin/semantic-cache/config"}: "getConfig",
		{"GET", "/api/admin/virtual-keys"}:          "listVirtualKeys",
	}
	for k, exp := range want {
		if got := opID(routes, k[0], k[1]); got != exp {
			t.Errorf("%s %s → %q, want %q", k[0], k[1], got, exp)
		}
	}
	// Within a kind, every operationId is unique.
	seen := map[string]map[string]bool{}
	for _, r := range routes {
		kind := deriveKind(r.Path, "/api/admin")
		if seen[kind] == nil {
			seen[kind] = map[string]bool{}
		}
		if seen[kind][r.OperationID] {
			t.Fatalf("duplicate operationId %q in kind %q", r.OperationID, kind)
		}
		seen[kind][r.OperationID] = true
	}
}

func TestAssignOperationIDsFallbackUnique(t *testing.T) {
	// Two routes that collapse to the same base AND the same disambiguator token
	// must still end unique via the numeric fallback.
	routes := []route{
		{Method: "POST", Path: "/api/admin/x/a/run", handlerName: "Do"},
		{Method: "POST", Path: "/api/admin/x/b/run", handlerName: "Do"},
	}
	assignOperationIDs(routes, "/api/admin")
	if routes[0].OperationID == routes[1].OperationID {
		t.Fatalf("collision not broken: both %q", routes[0].OperationID)
	}
}

func TestPathAndTokenHelpers(t *testing.T) {
	if got := lastPathToken("/api/admin/hooks/:id/dry-run"); got != "dry-run" {
		t.Fatalf("lastPathToken = %q", got)
	}
	if got := lastPathToken("/api/admin/nodes/:id/{x}"); got != "nodes" {
		t.Fatalf("lastPathToken should skip params, got %q", got)
	}
	if got := pascalToken("dry-run"); got != "DryRun" {
		t.Fatalf("pascalToken = %q", got)
	}
	if got := disambiguate("hookTest", "test"); got != "hookTest" {
		t.Fatalf("disambiguate must skip a tail the base already ends with, got %q", got)
	}
	if got := disambiguate("updateRoutingRule", "PUT"); got != "updateRoutingRulePut" {
		t.Fatalf("disambiguate = %q", got)
	}
	if got := disambiguate("x", ""); got != "x" {
		t.Fatalf("empty token keeps base, got %q", got)
	}
}
