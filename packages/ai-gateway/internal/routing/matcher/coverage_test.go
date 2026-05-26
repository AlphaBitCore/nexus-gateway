package matcher

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// matcher.go — resolveField / equals / numCompare / toFloat / evalRegex /
// evalIn / logical operators (non-array fail paths) / MatchGlob extras.

// TestResolveField_AllPaths walks every named path in resolveField, including
// the nil-VK guard for every virtualKey.* path and the headers.* prefix.
func TestResolveField_AllPaths(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Tier", "enterprise")
	ctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{
			ID:              "gpt-4",
			Type:            "chat",
			ProviderID:      "openai",
			ProviderModelID: "gpt-4-0613",
		},
		EndpointType: "chat",
		VirtualKey: &core.VKContext{
			ID:             "vk-1",
			Name:           "eng",
			ProjectID:      "proj",
			OrganizationID: "org",
			SourceApp:      "web",
		},
		Headers: core.NewSafeHeaders(headers),
	}

	cases := []struct {
		path string
		want any
	}{
		{"requestedModel.id", "gpt-4"},
		{"requestedModel.type", "chat"},
		{"requestedModel.providerId", "openai"},
		{"requestedModel.providerModelId", "gpt-4-0613"},
		{"endpointType", "chat"},
		{"virtualKey.id", "vk-1"},
		{"virtualKey.name", "eng"},
		{"virtualKey.projectId", "proj"},
		{"virtualKey.organizationId", "org"},
		{"virtualKey.sourceApp", "web"},
		{"headers.X-Tier", "enterprise"},
		{"headers.x-tier", "enterprise"}, // case-insensitive
		{"unknown.field", nil},
		{"headers.x-missing", ""},
	}
	for _, tc := range cases {
		got := resolveField(tc.path, ctx)
		if got != tc.want {
			t.Errorf("resolveField(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestResolveField_NilVK_ReturnsNil locks every virtualKey.* path's nil-VK
// guard so a context without a VK cannot accidentally NPE in matcher eval.
func TestResolveField_NilVK_ReturnsNil(t *testing.T) {
	ctx := &core.RoutingContext{}
	for _, p := range []string{
		"virtualKey.id",
		"virtualKey.name",
		"virtualKey.projectId",
		"virtualKey.organizationId",
		"virtualKey.sourceApp",
	} {
		if got := resolveField(p, ctx); got != nil {
			t.Errorf("resolveField(%q) with nil VK = %v, want nil", p, got)
		}
	}
}

// TestEvalOperators_Comparison covers $gt/$gte/$lt/$lte numeric comparison
// paths through toFloat for every supported numeric kind.
func TestEvalOperators_Comparison(t *testing.T) {
	cases := []struct {
		name string
		expr map[string]any
		want bool
	}{
		{"$gt true", map[string]any{"$gt": 10}, true},
		{"$gt false equal", map[string]any{"$gt": 42}, false},
		{"$gte true equal", map[string]any{"$gte": 42}, true},
		{"$gte false", map[string]any{"$gte": 100}, false},
		{"$lt true", map[string]any{"$lt": 100}, true},
		{"$lt false equal", map[string]any{"$lt": 42}, false},
		{"$lte true equal", map[string]any{"$lte": 42}, true},
		{"$lte false", map[string]any{"$lte": 10}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// fieldValue=int(42), compared via numCompare.
			if got := evalOperators(42, tc.expr, &core.RoutingContext{}); got != tc.want {
				t.Errorf("evalOperators(%v) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestEvalOperators_NotWithNonMap_NoEffect: $not with a non-map opVal is
// silently skipped (defensive guard in evalOperators). Verify the
// surrounding predicate still succeeds.
func TestEvalOperators_NotWithNonMap_NoEffect(t *testing.T) {
	// $not with non-map value: the case body's type assertion fails
	// silently; the whole expression returns true.
	if !evalOperators("x", map[string]any{"$not": "not a map"}, &core.RoutingContext{}) {
		t.Error("$not with non-map opVal should be a no-op (returns true)")
	}
}

// TestEvalOperators_NestedAndOr exercises the $and/$or operator branches
// that live inside evalOperators (distinct from top-level EvaluateExpression).
func TestEvalOperators_NestedAndOr(t *testing.T) {
	ctx := &core.RoutingContext{RequestedModel: core.RequestedModel{Type: "chat"}}
	// $and inside an operator map.
	expr := map[string]any{"$and": []any{
		map[string]any{"requestedModel.type": "chat"},
	}}
	if !evalOperators("ignored", expr, ctx) {
		t.Error("$and inside evalOperators should pass")
	}
	// $and false.
	exprNo := map[string]any{"$and": []any{
		map[string]any{"requestedModel.type": "image"},
	}}
	if evalOperators("ignored", exprNo, ctx) {
		t.Error("$and inside evalOperators should fail when sub fails")
	}
	// $or true / false.
	if !evalOperators("ignored", map[string]any{"$or": []any{
		map[string]any{"requestedModel.type": "chat"},
	}}, ctx) {
		t.Error("$or inside evalOperators should pass")
	}
	if evalOperators("ignored", map[string]any{"$or": []any{
		map[string]any{"requestedModel.type": "image"},
	}}, ctx) {
		t.Error("$or inside evalOperators should fail when none match")
	}
}

// TestLogicalAndOr_NonArray pins the defensive guard: when the operand
// is not a []any (e.g. accidental object instead of array), the helper
// returns false.
func TestLogicalAndOr_NonArray(t *testing.T) {
	ctx := &core.RoutingContext{}
	if evalLogicalAnd(map[string]any{"not": "array"}, ctx) {
		t.Error("$and with non-array operand must return false")
	}
	if evalLogicalOr(map[string]any{"not": "array"}, ctx) {
		t.Error("$or with non-array operand must return false")
	}
}

// TestLogicalOr_NonMapElement covers the inner ok-cast branch — when a
// child of $or is not a map, it's skipped (and overall $or fails if no
// other element matched).
func TestLogicalOr_NonMapElement(t *testing.T) {
	ctx := &core.RoutingContext{}
	if evalLogicalOr([]any{"not-a-map", 42}, ctx) {
		t.Error("$or with only non-map elements should return false")
	}
	if evalLogicalAnd([]any{"not-a-map"}, ctx) != true {
		// non-map elements in $and are skipped (cast fails); empty effective list -> true
		t.Error("$and with only non-map elements should return true (vacuous)")
	}
}

// TestEvalIn_NonArrayOpVal pins the early-exit guard.
func TestEvalIn_NonArrayOpVal(t *testing.T) {
	if evalIn("x", "not-an-array") {
		t.Error("$in with non-array operand must return false")
	}
}

// TestEvalRegex_NonStringOpVal pins the early-exit guard.
func TestEvalRegex_NonStringOpVal(t *testing.T) {
	if evalRegex("x", 42) {
		t.Error("$regex with non-string operand must return false")
	}
}

// TestEvalRegex_InvalidPattern_NoMatch: a syntactically invalid regex
// must not panic; it returns no-match via getCachedRegex -> nil.
func TestEvalRegex_InvalidPattern_NoMatch(t *testing.T) {
	if evalRegex("anything", "[unterminated") {
		t.Error("invalid regex pattern must yield no match")
	}
}

// TestGetCachedRegex_EvictionAtCapacity walks the maxRegexCache eviction
// path: once the cache hits its cap, the next compile resets the map.
// We exercise this deterministically by inserting maxRegexCache + 1 unique
// patterns and verifying the most recent one is retrievable (i.e. the
// cache did not silently break).
func TestGetCachedRegex_EvictionAtCapacity(t *testing.T) {
	// Reset cache so the test is hermetic against neighbor tests.
	regexMu.Lock()
	regexCache = make(map[string]*regexp.Regexp)
	regexMu.Unlock()

	for i := range maxRegexCache + 1 {
		p := "^test-pattern-" + intToStr(i) + "$"
		if r := getCachedRegex(p); r == nil {
			t.Fatalf("getCachedRegex(%q) returned nil", p)
		}
	}
	// After eviction the latest pattern should still be retrievable.
	if r := getCachedRegex("^test-pattern-" + intToStr(maxRegexCache) + "$"); r == nil {
		t.Error("post-eviction lookup of most recent pattern returned nil")
	}
}

// intToStr is a tiny test-local helper; we deliberately avoid strconv
// import noise since the surrounding file uses strings.* heavily.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestEquals_AllTypedBranches: equals has a per-type fast path for
// string / float64 / bool / int / int64, then falls back to fmt.Sprintf.
// Exercise each path's match + mismatch (mismatched type triggers the
// fallback Sprintf compare).
func TestEquals_AllTypedBranches(t *testing.T) {
	// string == string
	if !equals("a", "a") || equals("a", "b") {
		t.Error("string equality broken")
	}
	// float64 == float64
	if !equals(1.5, 1.5) || equals(1.5, 2.5) {
		t.Error("float64 equality broken")
	}
	// bool == bool
	if !equals(true, true) || equals(true, false) {
		t.Error("bool equality broken")
	}
	// int == int
	if !equals(3, 3) || equals(3, 4) {
		t.Error("int equality broken")
	}
	// int64 == int64
	if !equals(int64(7), int64(7)) || equals(int64(7), int64(8)) {
		t.Error("int64 equality broken")
	}
	// Mismatched typed pairs go through fmt.Sprintf fallback.
	// 1 == "1" via Sprintf("%v") -> "1" == "1".
	if !equals(1, "1") {
		t.Error("mismatched-type equals should fall back to Sprintf and match \"1\"")
	}
	// Disjoint types unequal at Sprintf level.
	if equals(true, "false") {
		t.Error("Sprintf fallback should differentiate true / \"false\"")
	}
}

// TestNumCompare_NonNumericFails: numCompare returns false if either side
// is not coercible to float64.
func TestNumCompare_NonNumericFails(t *testing.T) {
	gt := func(a, b float64) bool { return a > b }
	if numCompare("not-a-number", 1, gt) {
		t.Error("numCompare with non-numeric left must return false")
	}
	if numCompare(1, "not-a-number", gt) {
		t.Error("numCompare with non-numeric right must return false")
	}
}

// TestToFloat_AllNumericTypes covers each branch of the type switch.
func TestToFloat_AllNumericTypes(t *testing.T) {
	cases := []struct {
		in     any
		want   float64
		wantOK bool
	}{
		{float64(1.5), 1.5, true},
		{float32(2.5), 2.5, true},
		{int(3), 3, true},
		{int64(4), 4, true},
		{json.Number("5.5"), 5.5, true},
		{json.Number("not-a-number"), 0, false},
		{"string", 0, false}, // unsupported type
		{nil, 0, false},
	}
	for _, tc := range cases {
		got, ok := toFloat(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("toFloat(%v) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// enumerate.go — depth limit + single-success-with-nil-lookup paths.

// TestEnumerate_DepthLimit_ReturnsNil triggers the depth guard by stacking
// fallback nodes deeper than enumerateDepthLimit. Once exceeded, the
// recursive call returns nil and the parent fallback yields an empty slice.
func TestEnumerate_DepthLimit_ReturnsNil(t *testing.T) {
	// Build a fallback nest of length depthLimit+2.
	node := core.StrategyNode{Type: "single", ProviderID: "x", ModelID: "y"}
	for range enumerateDepthLimit + 2 {
		node = core.StrategyNode{Type: "fallback", Targets: []core.StrategyNode{node}}
	}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup)
	// At least the outermost levels exceed depthLimit and return nil.
	// The total branches should be 0 (nothing reaches the terminal).
	if len(branches) != 0 {
		t.Errorf("expected empty branches when depth exceeds limit, got %d", len(branches))
	}
}

// TestEnumerate_Single_LookupReturnsNilNoError covers the explainLookupErr
// branch where err==nil but target==nil (rare but defended against).
func TestEnumerate_Single_LookupReturnsNilNoError(t *testing.T) {
	nilLookup := func(_ context.Context, _, _ string) (*core.RoutingTarget, error) {
		return nil, nil
	}
	node := core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, nilLookup)
	if len(branches) != 1 {
		t.Fatalf("expected 1 branch even on nil/nil lookup, got %d", len(branches))
	}
	if !strings.Contains(branches[0].Note, "nil target") {
		t.Errorf("expected nil-target note, got %q", branches[0].Note)
	}
}

// TestEnumerate_ABSplit_LookupNilNoError exercises singleBranch with
// a nil/nil lookup result so the explainLookupErr nil-error branch fires.
func TestEnumerate_ABSplit_ZeroWeight_ReturnsNil(t *testing.T) {
	node := core.StrategyNode{Type: "ab_split", ABTargets: []core.ABTarget{
		{ProviderID: "p", ModelID: "m", Weight: 0},
	}}
	if branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup); branches != nil {
		t.Errorf("zero-total-weight ab_split should return nil, got %+v", branches)
	}
}

// TestEnumerate_UnknownNodeType_ReturnsNil covers the default arm of the
// enumerate switch.
func TestEnumerate_UnknownNodeType_ReturnsNil(t *testing.T) {
	node := core.StrategyNode{Type: "no-such-strategy"}
	if branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup); branches != nil {
		t.Errorf("unknown node type should return nil, got %+v", branches)
	}
}

// TestEnumerate_LoadbalanceEmpty_ReturnsNil exercises the empty-Weighted
// guard in enumerateWeighted.
func TestNarrowingEngine_Filter_DropsByVKAllowed(t *testing.T) {
	eng := &NarrowingEngine{}
	state := EmptyNarrowingState()
	targets := []core.RoutingTarget{
		{ProviderID: "openai", ModelID: "gpt-4", ProviderModelID: "gpt-4"},
		{ProviderID: "anthropic", ModelID: "claude-3", ProviderModelID: "claude-3"},
	}
	rctx := &core.RoutingContext{VirtualKey: &core.VKContext{AllowedModels: []store.AllowedModelRef{
		{ProviderID: "openai", ModelID: "gpt-*"},
	}}}
	out := eng.Filter(targets, state, rctx)
	if len(out) != 1 || out[0].ProviderID != "openai" {
		t.Errorf("VK allowedModels filter wrong: %+v", out)
	}
}

// TestSortedKeys_NilReturnsNil and EmptyNarrowing toSet
func TestSortedKeys_NilReturnsNil(t *testing.T) {
	if got := sortedKeys(nil); got != nil {
		t.Errorf("sortedKeys(nil) = %v, want nil", got)
	}
}

// TestMergePolicy_EmptyNodeNoOp: passing a fresh policy node with no
// allow/deny lists must leave state unchanged.
