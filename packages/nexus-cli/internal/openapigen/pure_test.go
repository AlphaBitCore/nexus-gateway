package openapigen

import (
	"go/types"
	"reflect"
	"testing"
)

func TestJoinPath(t *testing.T) {
	cases := []struct{ prefix, lit, want string }{
		{"/api/admin", "/widgets", "/api/admin/widgets"},
		{"/api/admin/", "/widgets", "/api/admin/widgets"},
		{"/api/admin", "widgets", "/api/admin/widgets"},
		{"/api/admin", "", "/api/admin"},
		{"", "", "/"},
		{"/api/admin", "/widgets/:id", "/api/admin/widgets/:id"},
	}
	for _, c := range cases {
		if got := joinPath(c.prefix, c.lit); got != c.want {
			t.Errorf("joinPath(%q,%q)=%q want %q", c.prefix, c.lit, got, c.want)
		}
	}
}

func TestEchoToOpenAPIPath(t *testing.T) {
	got, params := echoToOpenAPIPath("/api/admin/widgets/:id/sub/:sub")
	if got != "/api/admin/widgets/{id}/sub/{sub}" {
		t.Errorf("path=%q", got)
	}
	if !reflect.DeepEqual(params, []string{"id", "sub"}) {
		t.Errorf("params=%v", params)
	}
	got, params = echoToOpenAPIPath("/files/*")
	if got != "/files/{wildcard}" || !reflect.DeepEqual(params, []string{"wildcard"}) {
		t.Errorf("wildcard path=%q params=%v", got, params)
	}
}

func TestDeriveKind(t *testing.T) {
	cases := []struct{ path, base, want string }{
		{"/api/admin/quota-policies", "/api/admin", "quota-policies"},
		{"/api/admin/quota-policies/{id}", "/api/admin", "quota-policies"},
		{"/api/admin/cache/global", "/api/admin", "cache"},
		{"/api/admin", "/api/admin", ""},
		{"/api/admin/{id}", "/api/admin", ""},
		{"/other/thing", "/api/admin", "other"},
	}
	for _, c := range cases {
		if got := deriveKind(c.path, c.base); got != c.want {
			t.Errorf("deriveKind(%q)=%q want %q", c.path, got, c.want)
		}
	}
}

func TestOperationID(t *testing.T) {
	if got := operationID("CreateQuotaPolicy", "POST", "/x"); got != "createQuotaPolicy" {
		t.Errorf("named=%q", got)
	}
	if got := operationID("", "GET", "/api/admin/widgets/{id}"); got != "getApiAdminWidgetsId" {
		t.Errorf("synth=%q", got)
	}
}

func TestDeriveTier(t *testing.T) {
	cases := []struct {
		method, iam string
		want        Tier
	}{
		{"GET", "", tierAuto},
		{"POST", "", tierConfirm},
		{"GET", "iam.R.Action(iam.VerbCreate)", tierConfirm},
		{"DELETE", "iam.R.Action(iam.VerbRead)", tierAuto},
		{"GET", "iam.R.Action(iam.VerbList)", tierAuto},
		{"PUT", "iam.R.Action(iam.VerbUpdate)", tierConfirm},
	}
	for _, c := range cases {
		if got := deriveTier(c.method, c.iam); got != c.want {
			t.Errorf("deriveTier(%q,%q)=%q want %q", c.method, c.iam, got, c.want)
		}
	}
}

func TestJSONField(t *testing.T) {
	cases := []struct {
		tag                string
		field              string
		wantName           string
		wantOmit, wantSkip bool
	}{
		{`json:"name"`, "Name", "name", false, false},
		{`json:"name,omitempty"`, "Name", "name", true, false},
		{`json:"-"`, "Secret", "", false, true},
		{`json:",omitempty"`, "Name", "Name", true, false},
		{"", "Name", "Name", false, false},
	}
	for _, c := range cases {
		name, omit, skip := jsonField(reflect.StructTag(c.tag), c.field)
		if name != c.wantName || omit != c.wantOmit || skip != c.wantSkip {
			t.Errorf("jsonField(%q,%q)=(%q,%v,%v) want (%q,%v,%v)",
				c.tag, c.field, name, omit, skip, c.wantName, c.wantOmit, c.wantSkip)
		}
	}
}

func TestComponentName(t *testing.T) {
	if got := componentName("example.com/cp/api", "Widget"); got != "api_Widget" {
		t.Errorf("got %q", got)
	}
	if got := componentName("", "Anon"); got != "Anon" {
		t.Errorf("got %q", got)
	}
}

// TestClaimComponentDisambiguates proves the collision guard: two distinct types
// whose package last-segment + name collide (a/cache.Config vs b/cache.Config) get
// distinct component keys, so the second is described, not silently aliased to the
// first. A non-colliding name is returned unchanged.
func TestClaimComponentDisambiguates(t *testing.T) {
	b := newSchemaBuilder()
	first := b.claimComponent("svc/a/cache", "Config")
	if first != "cache_Config" {
		t.Fatalf("first claim should be the plain name, got %q", first)
	}
	b.components[first] = newOMap() // simulate registration of the first type
	second := b.claimComponent("svc/b/cache", "Config")
	if second == first {
		t.Fatalf("a colliding distinct type must get a distinct key, got %q for both", second)
	}
	if second != "cache_Config_2" {
		t.Fatalf("disambiguated key should suffix _2, got %q", second)
	}
	// A non-colliding name is returned unchanged.
	if got := b.claimComponent("svc/quota", "Policy"); got != "quota_Policy" {
		t.Fatalf("non-colliding claim should be unchanged, got %q", got)
	}
}

func TestBasicSchemaAndNull(t *testing.T) {
	cases := []struct {
		kind     types.BasicKind
		wantType string
		wantFmt  string
	}{
		{types.String, "string", ""},
		{types.Bool, "boolean", ""},
		{types.Int, "integer", ""},
		{types.Int64, "integer", "int64"},
		{types.Float64, "number", ""},
	}
	for _, c := range cases {
		s := basicSchema(types.Typ[c.kind])
		ty, _ := s.Get("type")
		if ty != c.wantType {
			t.Errorf("kind %v type=%v want %v", c.kind, ty, c.wantType)
		}
		if c.wantFmt != "" {
			if f, _ := s.Get("format"); f != c.wantFmt {
				t.Errorf("kind %v format=%v want %v", c.kind, f, c.wantFmt)
			}
		}
	}
	// withNull promotes a concrete type into [type,"null"].
	n := withNull(basicSchema(types.Typ[types.String]))
	ty, _ := n.Get("type")
	if !reflect.DeepEqual(ty, []string{"string", "null"}) {
		t.Errorf("withNull type=%v", ty)
	}
	// withNull on a $ref-only schema is a no-op.
	ref := newOMap().Set("$ref", "#/x")
	if withNull(ref); ref.Len() != 1 {
		t.Errorf("withNull mutated ref schema")
	}
}

func TestSchemaForCompositeTypes(t *testing.T) {
	b := newSchemaBuilder()
	// slice of string
	arr := b.schemaFor(types.NewSlice(types.Typ[types.String]))
	if ty, _ := arr.Get("type"); ty != "array" {
		t.Errorf("slice type=%v", ty)
	}
	// []byte -> string/byte
	bs := b.schemaFor(types.NewSlice(types.Typ[types.Byte]))
	if f, _ := bs.Get("format"); f != "byte" {
		t.Errorf("[]byte format=%v", f)
	}
	// map -> object additionalProperties
	m := b.schemaFor(types.NewMap(types.Typ[types.String], types.Typ[types.Int]))
	if _, ok := m.Get("additionalProperties"); !ok {
		t.Errorf("map missing additionalProperties")
	}
	// pointer -> nullable
	p := b.schemaFor(types.NewPointer(types.Typ[types.String]))
	if ty, _ := p.Get("type"); !reflect.DeepEqual(ty, []string{"string", "null"}) {
		t.Errorf("pointer type=%v", ty)
	}
	// interface -> any (empty schema)
	iface := b.schemaFor(types.NewInterfaceType(nil, nil).Complete())
	if iface.Len() != 0 {
		t.Errorf("interface schema not empty: %v", iface.keys)
	}
	// nil -> any
	if b.schemaFor(nil).Len() != 0 {
		t.Errorf("nil schema not empty")
	}
}

func TestSchemaForExoticTypes(t *testing.T) {
	b := newSchemaBuilder()
	// fixed-size array -> array schema.
	arr := b.schemaFor(types.NewArray(types.Typ[types.String], 3))
	if ty, _ := arr.Get("type"); ty != "array" {
		t.Errorf("array type=%v", ty)
	}
	// channel -> unresolved (default case).
	ch := b.schemaFor(types.NewChan(types.SendRecv, types.Typ[types.Int]))
	if _, ok := ch.Get("x-nexus-unresolved-type"); !ok {
		t.Errorf("chan should be unresolved: %v", ch.keys)
	}
	// complex basic kind -> unresolved basic.
	cx := basicSchema(types.Typ[types.Complex128])
	if _, ok := cx.Get("x-nexus-unresolved-basic"); !ok {
		t.Errorf("complex should be unresolved basic")
	}
}

func TestLowerUpperFirstEmpty(t *testing.T) {
	if lowerFirst("") != "" || upperFirst("") != "" {
		t.Error("empty string should pass through")
	}
	if lowerFirst("X") != "x" || upperFirst("x") != "X" {
		t.Error("first-letter case change")
	}
}

func TestOMapOrderAndSetIf(t *testing.T) {
	m := newOMap()
	m.Set("b", 1).Set("a", 2).Set("b", 3) // overwrite keeps position
	m.SetIf(false, "skip", 9).SetIf(true, "c", 4)
	if !reflect.DeepEqual(m.keys, []string{"b", "a", "c"}) {
		t.Errorf("keys=%v", m.keys)
	}
	if v, _ := m.Get("b"); v != 3 {
		t.Errorf("overwrite v=%v", v)
	}
	if _, ok := m.Get("skip"); ok {
		t.Errorf("SetIf(false) should not set")
	}
	if m.Len() != 3 {
		t.Errorf("len=%d", m.Len())
	}
}

func TestStatusTextAndSummary(t *testing.T) {
	if statusText(0) != "Unresolved status" {
		t.Error("status 0")
	}
	if statusText(200) != "OK" {
		t.Error("status 200")
	}
	if statusText(799) != "799" {
		t.Error("status 799")
	}
	if generatedSummary("GET", "widgets") != "Read widgets" {
		t.Error("summary GET")
	}
	if generatedSummary("HEAD", "widgets") != "HEAD widgets" {
		t.Error("summary HEAD fallback")
	}
}
