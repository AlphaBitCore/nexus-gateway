package device

import (
	"testing"
)

func mkDev() *Device {
	return &Device{
		OS:               "darwin",
		OSVersion:        "26.3.1",
		AgentVersion:     "1.5.2",
		Hostname:         "mac-eng-01.corp.local",
		PrimaryIP:        "10.32.4.17",
		PhysicalID:       "30e895b22c515478ddfd955b48e957b8",
		Status:           "online",
		BoundUserID:      "user-alice",
		BoundUserOrgPath: "corp/finance/treasury",
		EnrolledAtSec:    1_700_000_000,
		LastHeartbeatSec: 1_710_000_000,
		Metadata:         map[string]string{"team": "treasury", "office": "sg"},
	}
}

func TestEvaluate_All(t *testing.T) {
	dev := mkDev()
	p := Predicate{All: []Leaf{
		{Field: "os", Op: "eq", Value: "darwin"},
		{Field: "primaryIp", Op: "cidr", Value: "10.32.0.0/16"},
		{Field: "boundUserOrgPath", Op: "prefix", Value: "corp/finance/"},
	}}
	ok, err := Evaluate(p, dev, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Error("expected match for all-true predicate")
	}
}

func TestEvaluate_AllFailsFast(t *testing.T) {
	dev := mkDev()
	p := Predicate{All: []Leaf{
		{Field: "os", Op: "eq", Value: "windows"}, // false → short-circuit
		{Field: "primaryIp", Op: "cidr", Value: "10.32.0.0/16"},
	}}
	ok, _ := Evaluate(p, dev, 0)
	if ok {
		t.Error("AND with one false leaf should not match")
	}
}

func TestEvaluate_Any(t *testing.T) {
	dev := mkDev()
	p := Predicate{Any: []Leaf{
		{Field: "os", Op: "eq", Value: "windows"},
		{Field: "os", Op: "eq", Value: "darwin"}, // true → short-circuit
	}}
	ok, _ := Evaluate(p, dev, 0)
	if !ok {
		t.Error("OR with one true leaf should match")
	}
}

func TestEvaluate_EmptyMatchesNothing(t *testing.T) {
	dev := mkDev()
	p := Predicate{}
	ok, err := Evaluate(p, dev, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("empty predicate should match nothing")
	}
}

func TestEvaluate_BothAllAndAnyRejected(t *testing.T) {
	dev := mkDev()
	p := Predicate{
		All: []Leaf{{Field: "os", Op: "eq", Value: "darwin"}},
		Any: []Leaf{{Field: "status", Op: "eq", Value: "online"}},
	}
	_, err := Evaluate(p, dev, 0)
	if err == nil {
		t.Error("top-level all+any is a shape error")
	}
}

func TestEvaluate_Operators(t *testing.T) {
	dev := mkDev()
	cases := []struct {
		leaf Leaf
		want bool
	}{
		{Leaf{Field: "os", Op: "eq", Value: "darwin"}, true},
		{Leaf{Field: "os", Op: "ne", Value: "darwin"}, false},
		{Leaf{Field: "os", Op: "in", Value: []any{"darwin", "linux"}}, true},
		{Leaf{Field: "os", Op: "in", Value: []any{"windows"}}, false},
		{Leaf{Field: "os", Op: "nin", Value: []any{"windows"}}, true},
		{Leaf{Field: "hostname", Op: "prefix", Value: "mac-"}, true},
		{Leaf{Field: "hostname", Op: "regex", Value: `^mac-.*\.local$`}, true},
		{Leaf{Field: "primaryIp", Op: "cidr", Value: "10.32.0.0/16"}, true},
		{Leaf{Field: "primaryIp", Op: "cidr", Value: "10.33.0.0/16"}, false},
		// Semver comparisons.
		{Leaf{Field: "agentVersion", Op: "ge", Value: "1.5.0"}, true},
		{Leaf{Field: "agentVersion", Op: "ge", Value: "2.0.0"}, false},
		{Leaf{Field: "agentVersion", Op: "lt", Value: "2.0.0"}, true},
		// Metadata escape hatch.
		{Leaf{Field: "metadata.team", Op: "eq", Value: "treasury"}, true},
		{Leaf{Field: "metadata.missing", Op: "eq", Value: ""}, true}, // missing → empty
	}
	for _, c := range cases {
		p := Predicate{All: []Leaf{c.leaf}}
		got, err := Evaluate(p, dev, 0)
		if err != nil {
			t.Errorf("%+v: err %v", c.leaf, err)
			continue
		}
		if got != c.want {
			t.Errorf("%+v: got %v want %v", c.leaf, got, c.want)
		}
	}
}

func TestFieldValue_AllExportedFields(t *testing.T) {
	// Each field name in fieldValue's switch is referenced by a YAML
	// predicate operator path. A refactor that drops a case would
	// silently turn that field into "unknown field" → predicate-shape
	// error → policy effectively disabled. Pin every supported field.
	dev := mkDev()
	cases := []struct {
		field string
		want  any
	}{
		{"os", "darwin"},
		{"osVersion", "26.3.1"},
		{"agentVersion", "1.5.2"},
		{"hostname", "mac-eng-01.corp.local"},
		{"primaryIp", "10.32.4.17"},
		{"physicalId", "30e895b22c515478ddfd955b48e957b8"},
		{"status", "online"},
		{"boundUserId", "user-alice"},
		{"boundUserOrgPath", "corp/finance/treasury"},
		{"enrolledAt", int64(1_700_000_000)},
		{"lastHeartbeat", int64(1_710_000_000)},
		{"idpGroup", ""}, // sentinel: resolved against IdpGroupIDs, not here
		{"tags", ""},     // same sentinel pattern
		{"metadata.team", "treasury"},
		{"metadata.missing", ""}, // present-but-empty (still ok=true)
	}
	for _, c := range cases {
		got, ok := fieldValue(c.field, dev)
		if !ok {
			t.Errorf("%s: got ok=false", c.field)
		}
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.field, got, c.want)
		}
	}
}

func TestFieldValue_UnknownReturnsNilFalse(t *testing.T) {
	dev := mkDev()
	v, ok := fieldValue("nonexistent.path", dev)
	if ok {
		t.Errorf("unknown field should return ok=false; got %v ok=true", v)
	}
}

func TestEq_Direct(t *testing.T) {
	// String-vs-string ✓
	if !eq("abc", "abc") {
		t.Errorf("string equals: got false")
	}
	// String vs non-string non-numeric → false
	if eq("abc", true) {
		t.Errorf("string vs bool should not match")
	}
	// Int vs float64 (JSON decodes numbers as float64).
	if !eq(int(42), float64(42.0)) {
		t.Errorf("int vs float64 same value: got false")
	}
	// Different numeric values.
	if eq(int(42), int(43)) {
		t.Errorf("int 42 vs 43: got true")
	}
	// Bool fields are not comparable.
	if eq(true, false) {
		t.Errorf("bool-bool comparison routed wrong")
	}
}

func TestToInt64_AllNumericTypes(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{int(42), 42, true},
		{int32(42), 42, true},
		{int64(42), 42, true},
		{float64(42.7), 42, true}, // truncates
		{"42", 0, false},
		{true, 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := toInt64(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("toInt64(%T %v): got (%d, %v) want (%d, %v)", c.in, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCmpInt_NonComparableErrors(t *testing.T) {
	// cmpInt rejects non-numeric, non-version inputs with an explicit
	// error — without it, predicates against string fields would
	// silently always evaluate false.
	_, err := cmpInt("hello", "world", "lt")
	if err == nil {
		t.Error("non-numeric values should error")
	}
}

func TestCmpInt_UnknownOpErrors(t *testing.T) {
	_, err := cmpInt(int(1), int(2), "nope")
	if err == nil {
		t.Error("unknown op should error")
	}
}

func TestCmpInt_VersionPathBranchesAllOps(t *testing.T) {
	// Each comparison op has its own branch in the version path; pin
	// all four so a refactor can't silently drop one.
	cases := []struct {
		a, b, op string
		want     bool
	}{
		{"1.0.0", "2.0.0", "lt", true},
		{"2.0.0", "1.0.0", "lt", false},
		{"1.0.0", "1.0.0", "le", true},
		{"1.0.1", "1.0.0", "le", false},
		{"2.0.0", "1.0.0", "gt", true},
		{"1.0.0", "1.0.0", "gt", false},
		{"1.0.0", "1.0.0", "ge", true},
		{"1.0.0", "2.0.0", "ge", false},
	}
	for _, c := range cases {
		got, err := cmpInt(c.a, c.b, c.op)
		if err != nil {
			t.Errorf("%s %s %s: %v", c.a, c.op, c.b, err)
		}
		if got != c.want {
			t.Errorf("%s %s %s: got %v want %v", c.a, c.op, c.b, got, c.want)
		}
	}
}

func TestLooksLikeVersion(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"1.0.0", true},
		{"1.0", true},
		{"1", false},        // no dot
		{"", false},         // empty
		{"v1.0", false},     // letter prefix
		{"1.0-beta", false}, // hyphen
	}
	for _, c := range cases {
		if got := looksLikeVersion(c.s); got != c.want {
			t.Errorf("looksLikeVersion(%q): got %v want %v", c.s, got, c.want)
		}
	}
}

func TestEvaluate_UnknownField(t *testing.T) {
	dev := mkDev()
	p := Predicate{All: []Leaf{{Field: "notARealField", Op: "eq", Value: "x"}}}
	_, err := Evaluate(p, dev, 0)
	if err == nil {
		t.Error("unknown field should be a shape error")
	}
}

func TestEvaluate_UnknownOp(t *testing.T) {
	dev := mkDev()
	p := Predicate{All: []Leaf{{Field: "os", Op: "matches-fuzzy", Value: "darwin"}}}
	_, err := Evaluate(p, dev, 0)
	if err == nil {
		t.Error("unknown op should be a shape error")
	}
}

func TestEvaluate_RelativeSecondsWithin(t *testing.T) {
	dev := mkDev()
	// Heartbeat 30s ago; "within 60s of now" → true.
	now := dev.LastHeartbeatSec + 30
	p := Predicate{All: []Leaf{{Field: "lastHeartbeat", Op: "relative_seconds_within", Value: 60}}}
	ok, err := Evaluate(p, dev, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Error("Δ=30 should be within 60s")
	}
	// "within 10s of now" → false (Δ=30 > 10).
	p = Predicate{All: []Leaf{{Field: "lastHeartbeat", Op: "relative_seconds_within", Value: 10}}}
	ok, _ = Evaluate(p, dev, now)
	if ok {
		t.Error("Δ=30 should not be within 10s")
	}
}

func TestEvaluate_IdpGroupMember(t *testing.T) {
	dev := mkDev()
	dev.IdpGroupIDs = []string{"scim-finance", "scim-treasury"}

	cases := []struct {
		leaf Leaf
		want bool
	}{
		{Leaf{Field: "idpGroup", Op: "idp_group_member", Value: "scim-finance"}, true},
		{Leaf{Field: "idpGroup", Op: "idp_group_member", Value: "scim-engineering"}, false},
		{Leaf{Field: "idpGroup", Op: "idp_group_member", Value: []any{"scim-engineering", "scim-finance"}}, true},
		{Leaf{Field: "idpGroup", Op: "idp_group_member", Value: []any{"scim-engineering"}}, false},
	}
	for _, c := range cases {
		p := Predicate{All: []Leaf{c.leaf}}
		got, err := Evaluate(p, dev, 0)
		if err != nil {
			t.Errorf("%+v: err %v", c.leaf, err)
			continue
		}
		if got != c.want {
			t.Errorf("%+v: got %v want %v", c.leaf, got, c.want)
		}
	}

	// Wrong field name should be a shape error so operator typos
	// surface early at preview time.
	p := Predicate{All: []Leaf{{Field: "os", Op: "idp_group_member", Value: "scim-finance"}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("idp_group_member on non-idpGroup field should error")
	}
}

func TestEvaluate_TagsContains(t *testing.T) {
	dev := mkDev()
	dev.Tags = []string{"finance", "byod"}

	cases := []struct {
		leaf Leaf
		want bool
	}{
		{Leaf{Field: "tags", Op: "tags_contains", Value: "finance"}, true},
		{Leaf{Field: "tags", Op: "tags_contains", Value: "engineering"}, false},
		{Leaf{Field: "tags", Op: "tags_contains", Value: []any{"engineering", "finance"}}, true},
		{Leaf{Field: "tags", Op: "tags_contains_all", Value: []any{"finance", "byod"}}, true},
		{Leaf{Field: "tags", Op: "tags_contains_all", Value: []any{"finance", "engineering"}}, false},
	}
	for _, c := range cases {
		p := Predicate{All: []Leaf{c.leaf}}
		got, err := Evaluate(p, dev, 0)
		if err != nil {
			t.Errorf("%+v: err %v", c.leaf, err)
			continue
		}
		if got != c.want {
			t.Errorf("%+v: got %v want %v", c.leaf, got, c.want)
		}
	}

	// Wrong field name should error so typos surface at preview.
	p := Predicate{All: []Leaf{{Field: "os", Op: "tags_contains", Value: "x"}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("tags_contains on non-tags field should error")
	}
}

func TestEvaluate_EmptyIPNoMatch(t *testing.T) {
	dev := mkDev()
	dev.PrimaryIP = ""
	p := Predicate{All: []Leaf{{Field: "primaryIp", Op: "cidr", Value: "10.32.0.0/16"}}}
	ok, err := Evaluate(p, dev, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("empty ip should not match any cidr")
	}
}

// TestEvaluate_AnyNoMatch exercises the OR branch when every leaf is
// false — the existing TestEvaluate_Any only covers short-circuit on a
// true leaf, leaving the "fall through to return false" path uncovered.
func TestEvaluate_AnyNoMatch(t *testing.T) {
	dev := mkDev()
	p := Predicate{Any: []Leaf{
		{Field: "os", Op: "eq", Value: "windows"},
		{Field: "os", Op: "eq", Value: "linux"},
	}}
	ok, err := Evaluate(p, dev, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("OR with all false leaves should not match")
	}
}

// TestEvaluate_AnyPropagatesShapeError pins the rule that a malformed
// leaf inside an `any` block surfaces as an error rather than being
// silently skipped — a typo'd op in one OR clause must still fail
// loudly at preview time.
func TestEvaluate_AnyPropagatesShapeError(t *testing.T) {
	dev := mkDev()
	p := Predicate{Any: []Leaf{
		{Field: "os", Op: "matches-fuzzy", Value: "darwin"}, // unknown op
		{Field: "os", Op: "eq", Value: "darwin"},
	}}
	_, err := Evaluate(p, dev, 0)
	if err == nil {
		t.Error("Any: shape error in first leaf should propagate")
	}
}

// TestEvaluate_RegexShapeErrors covers both regex failure modes:
// empty pattern (operator typo) and malformed pattern (typo in
// character class) must surface as shape errors so the preview UI
// can flag the rule rather than silently matching nothing.
func TestEvaluate_RegexShapeErrors(t *testing.T) {
	dev := mkDev()
	// Empty regex.
	p := Predicate{All: []Leaf{{Field: "hostname", Op: "regex", Value: ""}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("empty regex should error")
	}
	// Malformed regex.
	p = Predicate{All: []Leaf{{Field: "hostname", Op: "regex", Value: "[unclosed"}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("malformed regex should error")
	}
}

// TestEvaluate_CIDRShapeErrors covers the cidr-op error paths.
// A malformed CIDR string is a predicate-shape error; a malformed IP
// on the device side is a silent non-match (the device is at fault,
// not the rule).
func TestEvaluate_CIDRShapeErrors(t *testing.T) {
	dev := mkDev()
	// Bad CIDR in the predicate value → shape error.
	p := Predicate{All: []Leaf{{Field: "primaryIp", Op: "cidr", Value: "not-a-cidr"}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("malformed cidr should error")
	}
	// Device with an unparsable IP (operator typo on enrollment) →
	// silent non-match, not error.
	dev.PrimaryIP = "not-an-ip"
	p = Predicate{All: []Leaf{{Field: "primaryIp", Op: "cidr", Value: "10.0.0.0/8"}}}
	ok, err := Evaluate(p, dev, 0)
	if err != nil {
		t.Fatalf("malformed device IP should not error: %v", err)
	}
	if ok {
		t.Error("malformed device IP should not match")
	}
}

// TestEvaluate_TagsContainsBadValue covers the two value-shape error
// branches: tags_contains given a non-string non-list, and
// tags_contains_all given a non-list. Both must surface as shape
// errors so YAML typos fail preview.
func TestEvaluate_TagsContainsBadValue(t *testing.T) {
	dev := mkDev()
	dev.Tags = []string{"finance"}
	// tags_contains with an int → shape error.
	p := Predicate{All: []Leaf{{Field: "tags", Op: "tags_contains", Value: 42}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("tags_contains with non-string non-list value should error")
	}
	// tags_contains_all with non-list value → shape error.
	p = Predicate{All: []Leaf{{Field: "tags", Op: "tags_contains_all", Value: "finance"}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("tags_contains_all with non-list value should error")
	}
	// tags_contains_all on a non-tags field → shape error.
	p = Predicate{All: []Leaf{{Field: "os", Op: "tags_contains_all", Value: []any{"x"}}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("tags_contains_all on non-tags field should error")
	}
}

// TestEvaluate_IdpGroupMemberBadValue covers the value-shape error
// branch: idp_group_member must reject scalar non-strings and
// non-list-non-string values (e.g. an int by accident in the YAML).
func TestEvaluate_IdpGroupMemberBadValue(t *testing.T) {
	dev := mkDev()
	dev.IdpGroupIDs = []string{"g1"}
	p := Predicate{All: []Leaf{{Field: "idpGroup", Op: "idp_group_member", Value: 42}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("idp_group_member with non-string non-list value should error")
	}
}

// TestEvaluate_RelativeSecondsWithinErrors covers the four error /
// no-match branches of the relative_seconds_within op:
//   - device field type mismatch (string instead of int64) → no match
//   - field present but zero (e.g. lastHeartbeat=0 for never-seen
//     device) → no match
//   - non-numeric value → shape error
//   - nowSec ≤ 0 (caller forgot to pass `now`) → shape error
func TestEvaluate_RelativeSecondsWithinErrors(t *testing.T) {
	dev := mkDev()

	// (1) lastHeartbeat=0 on a never-seen device → silent no-match.
	dev.LastHeartbeatSec = 0
	p := Predicate{All: []Leaf{{Field: "lastHeartbeat", Op: "relative_seconds_within", Value: 60}}}
	ok, err := Evaluate(p, dev, 1_710_000_000)
	if err != nil {
		t.Fatalf("zero timestamp should not error: %v", err)
	}
	if ok {
		t.Error("zero timestamp should not match")
	}

	// (2) Apply op to a non-time field (string) → got is not int64 → no match.
	p = Predicate{All: []Leaf{{Field: "os", Op: "relative_seconds_within", Value: 60}}}
	ok, _ = Evaluate(p, dev, 1_710_000_000)
	if ok {
		t.Error("relative_seconds_within on string field should not match")
	}

	// (3) Non-numeric value → shape error.
	dev.LastHeartbeatSec = 1_710_000_000
	p = Predicate{All: []Leaf{{Field: "lastHeartbeat", Op: "relative_seconds_within", Value: "soon"}}}
	if _, err := Evaluate(p, dev, 1_710_000_100); err == nil {
		t.Error("non-numeric value should error")
	}

	// (4) nowSec=0 → shape error (caller forgot to pass `now`).
	p = Predicate{All: []Leaf{{Field: "lastHeartbeat", Op: "relative_seconds_within", Value: 60}}}
	if _, err := Evaluate(p, dev, 0); err == nil {
		t.Error("nowSec=0 should error for relative_seconds_within")
	}
}

// TestInList_NonListValue pins that an `in` op with a scalar (non-list)
// value returns false — a YAML typo that drops the brackets must not
// silently coerce to a single-element list. (Predicate-shape errors
// here would be acceptable too, but the chosen semantics is "no match";
// pin it.)
func TestInList_NonListValue(t *testing.T) {
	if inList("darwin", "darwin") {
		t.Error("inList with scalar value should not match")
	}
}

// TestCmpInt_IntegerPathAllOps pins the four integer-comparison
// branches (separate from the version-string path). A refactor that
// drops one would silently turn the corresponding op into "always
// false" for numeric fields like enrolledAt / lastHeartbeat used as
// raw int64.
func TestCmpInt_IntegerPathAllOps(t *testing.T) {
	cases := []struct {
		a, b int64
		op   string
		want bool
	}{
		{1, 2, "lt", true},
		{2, 1, "lt", false},
		{1, 1, "le", true},
		{2, 1, "le", false},
		{2, 1, "gt", true},
		{1, 2, "gt", false},
		{1, 1, "ge", true},
		{1, 2, "ge", false},
	}
	for _, c := range cases {
		got, err := cmpInt(c.a, c.b, c.op)
		if err != nil {
			t.Errorf("%d %s %d: %v", c.a, c.op, c.b, err)
		}
		if got != c.want {
			t.Errorf("%d %s %d: got %v want %v", c.a, c.op, c.b, got, c.want)
		}
	}
}

// TestEvaluate_TagsContainsListNoMatch pins the path where
// tags_contains is given a list value and none of the candidates
// appear in the device tags — the function must return (false, nil),
// not error.
func TestEvaluate_TagsContainsListNoMatch(t *testing.T) {
	dev := mkDev()
	dev.Tags = []string{"finance"}
	p := Predicate{All: []Leaf{
		{Field: "tags", Op: "tags_contains", Value: []any{"engineering", "ops"}},
	}}
	ok, err := Evaluate(p, dev, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("tags_contains with list of non-matching tags should not match")
	}
}

// TestEvaluate_RelativeSecondsWithinFutureTimestamp covers the
// negative-delta branch: the device's timestamp is in the future
// relative to `now` (clock skew on the agent host). The op takes the
// absolute delta so it still evaluates symmetrically.
func TestEvaluate_RelativeSecondsWithinFutureTimestamp(t *testing.T) {
	dev := mkDev()
	dev.LastHeartbeatSec = 1_710_000_100 // 100s ahead of "now"
	now := int64(1_710_000_000)
	// |Δ|=100, within 200 → true.
	p := Predicate{All: []Leaf{{Field: "lastHeartbeat", Op: "relative_seconds_within", Value: 200}}}
	ok, err := Evaluate(p, dev, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Error("future timestamp within window (absolute delta) should match")
	}
	// |Δ|=100, within 50 → false.
	p = Predicate{All: []Leaf{{Field: "lastHeartbeat", Op: "relative_seconds_within", Value: 50}}}
	ok, _ = Evaluate(p, dev, now)
	if ok {
		t.Error("future timestamp outside window should not match")
	}
}

// TestCompareVersion_LongerB pins the branch where the right side has
// more dotted components than the left ("1.5" vs "1.5.1") — used when
// agentVersion is compared against a release that bumped the patch
// level.
func TestCompareVersion_LongerB(t *testing.T) {
	if compareVersion("1.5", "1.5.1") >= 0 {
		t.Error("1.5 should be less than 1.5.1")
	}
	if compareVersion("1.5.1", "1.5") <= 0 {
		t.Error("1.5.1 should be greater than 1.5")
	}
	// Equal when missing components are all zeros.
	if compareVersion("1.5.0", "1.5") != 0 {
		t.Error("1.5.0 should equal 1.5")
	}
}

// --- F-0282d: regex op compiles once per pattern (compile cache) ------------

func TestCompileRegex_CachesSamePointer(t *testing.T) {
	const pat = `^cache-test-[0-9]+$`
	regexCache.Delete(pat) // isolate from any prior test run

	re1, err := compileRegex(pat)
	if err != nil {
		t.Fatalf("first compile: %v", err)
	}
	re2, err := compileRegex(pat)
	if err != nil {
		t.Fatalf("second compile: %v", err)
	}
	if re1 != re2 {
		t.Errorf("same pattern must reuse the cached *regexp.Regexp; got distinct pointers")
	}
}

func TestCompileRegex_BadPatternNotCached(t *testing.T) {
	const bad = `(unterminated`
	regexCache.Delete(bad)
	if _, err := compileRegex(bad); err == nil {
		t.Fatal("expected compile error for bad pattern")
	}
	// An uncompilable pattern must NOT be cached, so a later valid edit of the
	// same string still recompiles and the error surfaces each time.
	if _, ok := regexCache.Load(bad); ok {
		t.Error("bad pattern must not be stored in the cache")
	}
	if _, err := compileRegex(bad); err == nil {
		t.Error("bad pattern should still error on a second call")
	}
}

func TestEvaluate_RegexOp_RepeatedCallsConsistent(t *testing.T) {
	// The regex op is exercised through Evaluate twice with the same pattern;
	// the cached compile must yield identical results (no stale/garbled state).
	d := mkDev()
	leaf := Leaf{Field: "hostname", Op: "regex", Value: `^mac-.*\.local$`}
	for i := range 3 {
		ok, err := Evaluate(Predicate{All: []Leaf{leaf}}, d, 0)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("call %d: expected match for %q", i, d.Hostname)
		}
	}
}
