package vendorbill

import (
	"encoding/json"
	"math"
	"testing"
)

// usdAmount exists because vendors disagree with their own docs about whether
// money is a JSON number or a decimal string. Both must decode to the same
// value; anything else must be a loud error, never a silent zero (a silent zero
// reads downstream as "the vendor billed nothing", i.e. 100% drift).
func TestUSDAmount_DecodesBothWireShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want float64
	}{
		{"bare number (docs shape)", `1.25`, 1.25},
		{"decimal string (OpenAI live shape)", `"1.25"`, 1.25},
		{"20-digit string keeps precision", `"15.46230655000000000000000000"`, 15.46230655},
		{"integer", `7`, 7},
		{"integer string", `"7"`, 7},
		{"zero", `0`, 0},
		{"null is zero", `null`, 0},
		{"whitespace-padded string", `"  2.50  "`, 2.50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a usdAmount
			if err := json.Unmarshal([]byte(c.raw), &a); err != nil {
				t.Fatalf("Unmarshal(%s): %v", c.raw, err)
			}
			if math.Abs(float64(a)-c.want) > 1e-9 {
				t.Fatalf("Unmarshal(%s) = %v, want %v", c.raw, float64(a), c.want)
			}
		})
	}
}

func TestUSDAmount_RejectsNonNumeric(t *testing.T) {
	for _, raw := range []string{`"abc"`, `"1.2.3"`, `""`, `"$1.25"`} {
		var a usdAmount
		if err := json.Unmarshal([]byte(raw), &a); err == nil {
			t.Errorf("Unmarshal(%s) must error, got %v — a silent 0 would read as 'vendor billed nothing'", raw, float64(a))
		}
	}
}

// Scope inference is what decides whether a row is comparable at all, so its
// boundaries are worth pinning explicitly.
func TestResolveScope(t *testing.T) {
	cases := []struct {
		name     string
		ids      []string
		wantKind string
		wantID   string
	}{
		{"single id narrows", []string{"p1", "p1"}, "project", "p1"},
		{"several ids collapse to org", []string{"p1", "p2"}, scopeOrg, ""},
		{"no ids at all is org", nil, scopeOrg, ""},
		{"empty ids (default workspace) is org", []string{"", ""}, scopeOrg, ""},
		{"empty mixed with one real id still narrows", []string{"", "p1"}, "project", "p1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, id := resolveScope("project", c.ids)
			if kind != c.wantKind || id != c.wantID {
				t.Fatalf("resolveScope(%v) = (%q,%q), want (%q,%q)", c.ids, kind, id, c.wantKind, c.wantID)
			}
		})
	}
}
