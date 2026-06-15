package openapigen

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fixtureOptions returns Options pointed at the testdata/cp fixture module,
// loaded in module mode (GOWORK=off) so it resolves against its own go.mod
// rather than the surrounding workspace.
func fixtureOptions(t *testing.T, out string) Options {
	t.Helper()
	src, err := filepath.Abs("testdata/cp")
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		SrcDir:  src,
		OutDir:  out,
		Version: "1.2.3",
		Env:     append(os.Environ(), "GOWORK=off"),
	}
}

func loadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func dig(t *testing.T, m any, keys ...string) any {
	t.Helper()
	cur := m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("dig: %q is not a map (got %T)", k, cur)
		}
		cur = mm[k]
	}
	return cur
}

// TestGenerate_MultipleRoots covers the multi-root walk: a registrar wired
// OUTSIDE RegisterAdminRoutes (RegisterStandaloneRoutes — the assistant's real
// shape) is discovered only when named as an explicit walk root, and its routes
// land under their own kind. This is the path the assistant relies on.
func TestGenerate_MultipleRoots(t *testing.T) {
	out := t.TempDir()
	opts := fixtureOptions(t, out)
	opts.RootFuncs = []string{"RegisterAdminRoutes", "RegisterStandaloneRoutes"}
	rep, err := Generate(opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The standalone kind is present ALONGSIDE the RegisterAdminRoutes tree —
	// the second root adds routes, it does not replace the first.
	if !containsStr(rep.Kinds, "standalone") {
		t.Fatalf("kinds=%v missing 'standalone' (second root not walked)", rep.Kinds)
	}
	if !containsStr(rep.Kinds, "widgets") {
		t.Errorf("kinds=%v missing 'widgets' (first root dropped)", rep.Kinds)
	}

	doc := loadYAML(t, filepath.Join(out, "standalone.yaml"))
	paths := dig(t, doc, "paths").(map[string]any)
	if _, ok := paths["/api/admin/standalone/ping"]; !ok {
		t.Errorf("standalone route not emitted under the base prefix; paths=%v", paths)
	}
}

// containsStr reports whether s is in xs.
func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestGenerate_Fixture(t *testing.T) {
	out := t.TempDir()
	rep, err := Generate(fixtureOptions(t, out))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Four kinds, alphabetically ordered.
	if got := strings.Join(rep.Kinds, ","); got != "extras,extras-sub,nested,widgets" {
		t.Fatalf("kinds=%q", got)
	}
	// widgets 7 (incl. the delegated-query-param events route) + nested 1 +
	// extras 4 + extras-sub 1 = 13 (root "/" + dynamic path are unresolved and
	// not counted).
	if rep.Routes != 13 {
		t.Errorf("routes=%d want 13", rep.Routes)
	}
	// Unresolved cases are reported, never silently dropped.
	for _, want := range []string{"non-literal path", "no derivable resource kind", "could not be resolved"} {
		if !containsSubstr(rep.Unresolved, want) {
			t.Errorf("unresolved=%v missing %q", rep.Unresolved, want)
		}
	}
	// Files: nested.yaml, widgets.yaml, _index.yaml.
	for _, name := range []string{"nested.yaml", "widgets.yaml", "_index.yaml"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing %s", name)
		}
	}

	doc := loadYAML(t, filepath.Join(out, "widgets.yaml"))
	if doc["openapi"] != "3.1.0" {
		t.Errorf("openapi=%v", doc["openapi"])
	}
	if dig(t, doc, "info", "version") != "1.2.3" {
		t.Errorf("version not stamped")
	}

	paths := dig(t, doc, "paths").(map[string]any)
	for _, p := range []string{"/api/admin/widgets", "/api/admin/widgets/{id}", "/api/admin/widgets/ping"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("missing path %s", p)
		}
	}

	// POST /widgets: confirm tier, requestBody, operationId, IAM action.
	post := dig(t, doc, "paths", "/api/admin/widgets", "post")
	if dig(t, post, "x-nexus-tier") != "confirm" {
		t.Errorf("post tier=%v", dig(t, post, "x-nexus-tier"))
	}
	if dig(t, post, "operationId") != "createWidget" {
		t.Errorf("post opId=%v", dig(t, post, "operationId"))
	}
	if dig(t, post, "x-nexus-iam-action") != "iam.ResourceWidget.Action(iam.VerbCreate)" {
		t.Errorf("post iam=%v", dig(t, post, "x-nexus-iam-action"))
	}
	schema := dig(t, post, "requestBody", "content", "application/json", "schema")
	props := dig(t, schema, "properties").(map[string]any)
	for _, want := range []string{"name", "description", "count", "enabled", "tags", "meta", "labels"} {
		if _, ok := props[want]; !ok {
			t.Errorf("request body missing property %s", want)
		}
	}
	if _, ok := props["secret"]; ok {
		t.Errorf(`json:"-" field "secret" should be excluded`)
	}
	required := toStringSet(dig(t, schema, "required"))
	if !required["name"] || !required["count"] || !required["tags"] || !required["meta"] || !required["labels"] {
		t.Errorf("required=%v missing non-pointer fields", required)
	}
	if required["description"] || required["enabled"] {
		t.Errorf("pointer/omitempty fields must not be required: %v", required)
	}
	// POST response codes from c.JSON: 201 + 400.
	if _, ok := dig(t, post, "responses").(map[string]any)["201"]; !ok {
		t.Errorf("missing 201 response")
	}
	if _, ok := dig(t, post, "responses").(map[string]any)["400"]; !ok {
		t.Errorf("missing 400 response")
	}

	// GET /widgets is auto tier and surfaces deduped query parameters.
	getList := dig(t, doc, "paths", "/api/admin/widgets", "get")
	if dig(t, getList, "x-nexus-tier") != "auto" {
		t.Errorf("get tier not auto")
	}
	qparams, ok := getList.(map[string]any)["parameters"].([]any)
	if !ok || len(qparams) != 2 {
		t.Fatalf("expected 2 deduped query params, got %v", getList.(map[string]any)["parameters"])
	}
	qnames := map[string]bool{}
	for _, p := range qparams {
		qnames[dig(t, p, "name").(string)] = true
		if dig(t, p, "in") != "query" || dig(t, p, "required") != false {
			t.Errorf("query param shape wrong: %v", p)
		}
	}
	if !qnames["scope"] || !qnames["status"] {
		t.Errorf("query params=%v want scope+status", qnames)
	}

	// GET /widgets/{id}/events delegates its query-string parsing to a shared
	// helper (widgetPaging); the generator must follow it so afterSeq + limit are
	// still documented (the listWorkflowRunEvents/pageParams pattern).
	evParams, ok := dig(t, doc, "paths", "/api/admin/widgets/{id}/events", "get").(map[string]any)["parameters"].([]any)
	if !ok {
		t.Fatalf("events endpoint has no parameters")
	}
	evNames := map[string]bool{}
	for _, p := range evParams {
		evNames[dig(t, p, "name").(string)] = true
	}
	if !evNames["afterSeq"] || !evNames["limit"] {
		t.Errorf("delegated query params=%v want afterSeq+limit (helper must be followed)", evNames)
	}

	// DELETE /widgets/{id} uses c.NoContent -> a 204 response with no body.
	del := dig(t, doc, "paths", "/api/admin/widgets/{id}", "delete", "responses").(map[string]any)
	if _, ok := del["204"]; !ok {
		t.Errorf("delete missing 204 (c.NoContent) response: %v", del)
	}
	if c := dig(t, del["204"], "content"); c != nil {
		t.Errorf("204 response must have no content body, got %v", c)
	}

	// Path param surfaced on /widgets/{id}.
	getByID := dig(t, doc, "paths", "/api/admin/widgets/{id}", "get")
	params, ok := getByID.(map[string]any)["parameters"].([]any)
	if !ok || len(params) != 1 || dig(t, params[0], "name") != "id" {
		t.Errorf("path param not surfaced: %v", getByID)
	}

	// Named response struct hoisted into components.
	comp := dig(t, doc, "components", "schemas")
	if _, ok := comp.(map[string]any)["api_Widget"]; !ok {
		t.Errorf("components missing api_Widget: %v", comp)
	}

	// Func-literal handler operationId synthesised from path.
	if dig(t, doc, "paths", "/api/admin/widgets/ping", "get", "operationId") != "getApiAdminWidgetsPing" {
		t.Errorf("ping opId=%v", dig(t, doc, "paths", "/api/admin/widgets/ping", "get", "operationId"))
	}

	// Index catalog lists kinds + operations with tiers.
	idx := loadYAML(t, filepath.Join(out, "_index.yaml"))
	if idx["basePrefix"] != "/api/admin" {
		t.Errorf("index basePrefix=%v", idx["basePrefix"])
	}
	kinds := idx["kinds"].([]any)
	if len(kinds) != 4 {
		t.Fatalf("index kinds=%d", len(kinds))
	}

	// CreateExtra is a named type, so the request body is a $ref into
	// components; its embedded *Base is promoted inline and time.Duration maps
	// to an integer. A non-constant status produces a "default" response.
	extras := loadYAML(t, filepath.Join(out, "extras.yaml"))
	createRef := dig(t, extras, "paths", "/api/admin/extras", "post", "requestBody", "content", "application/json", "schema")
	if dig(t, createRef, "$ref") != "#/components/schemas/api_CreateExtra" {
		t.Errorf("named request body should be a $ref, got %v", createRef)
	}
	cprops := dig(t, extras, "components", "schemas", "api_CreateExtra", "properties").(map[string]any)
	if _, ok := cprops["common"]; !ok {
		t.Errorf("embedded *Base.common not promoted: %v", cprops)
	}
	if _, ok := cprops["title"]; !ok {
		t.Errorf("CreateExtra.title missing")
	}
	if _, ok := cprops["internalID"]; ok {
		t.Errorf("unexported embedded field must be dropped")
	}
	if _, ok := cprops["timeout"]; !ok {
		t.Errorf("time.Duration field missing")
	}
	if _, ok := dig(t, extras, "paths", "/api/admin/extras", "get", "responses").(map[string]any)["default"]; !ok {
		t.Errorf("non-constant status should yield a default response")
	}
	// Bare (non-call) middleware leaves no IAM action on GET /extras.
	if _, ok := dig(t, extras, "paths", "/api/admin/extras", "get").(map[string]any)["x-nexus-iam-action"]; ok {
		t.Errorf("bare middleware should not yield an IAM action")
	}
}

func TestStringLit(t *testing.T) {
	cases := []struct {
		lit  *ast.BasicLit
		want string
		ok   bool
	}{
		{&ast.BasicLit{Kind: token.STRING, Value: `"/x"`}, "/x", true},
		{&ast.BasicLit{Kind: token.INT, Value: "42"}, "", false},
		{&ast.BasicLit{Kind: token.STRING, Value: `"unterminated`}, "", false},
	}
	for _, c := range cases {
		got, ok := stringLit(c.lit)
		if got != c.want || ok != c.ok {
			t.Errorf("stringLit(%q)=(%q,%v) want (%q,%v)", c.lit.Value, got, ok, c.want, c.ok)
		}
	}
	if _, ok := stringLit(&ast.Ident{Name: "x"}); ok {
		t.Error("non-literal should not parse")
	}
}

func TestGenerate_Errors(t *testing.T) {
	// Missing OutDir.
	opts := fixtureOptions(t, "")
	if _, err := Generate(opts); err == nil {
		t.Error("expected error for empty OutDir")
	}
	// Unloadable source dir.
	bad := Options{SrcDir: filepath.Join(t.TempDir(), "does-not-exist"), OutDir: t.TempDir(), Env: append(os.Environ(), "GOWORK=off")}
	if _, err := Generate(bad); err == nil {
		t.Error("expected error for bad SrcDir")
	}
}

func containsSubstr(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func toStringSet(v any) map[string]bool {
	out := map[string]bool{}
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}
