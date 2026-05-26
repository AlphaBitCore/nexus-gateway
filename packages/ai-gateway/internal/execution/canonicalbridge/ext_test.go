package canonicalbridge

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSetExt_GetExt_roundTripTwoProviders(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)

	var err error
	body, err = SetExt(body, "anthropic", "cache_read", 42)
	if err != nil {
		t.Fatal(err)
	}
	body, err = SetExt(body, "gemini", "safety_ratings", []string{"HARM"})
	if err != nil {
		t.Fatal(err)
	}

	if v := GetExt(body, "anthropic", "cache_read"); !v.Exists() || v.Int() != 42 {
		t.Fatalf("anthropic.cache_read: got %v", v.Raw)
	}
	arr := GetExt(body, "gemini", "safety_ratings")
	if !arr.Exists() || !arr.IsArray() || len(arr.Array()) != 1 || arr.Array()[0].Str != "HARM" {
		t.Fatalf("gemini.safety_ratings: got %v", arr.Raw)
	}

	// Unrelated top-level fields preserved
	if gjson.GetBytes(body, "model").String() != "x" {
		t.Fatal("model clobbered")
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	nexus, ok := m["nexus"].(map[string]any)
	if !ok {
		t.Fatalf("nexus missing: %#v", m)
	}
	ext, ok := nexus["ext"].(map[string]any)
	if !ok {
		t.Fatalf("nexus.ext missing under nexus: %#v", nexus)
	}
	if _, ok := ext["anthropic"].(map[string]any); !ok {
		t.Fatalf("nexus.ext.anthropic: %#v", ext["anthropic"])
	}
	if _, ok := ext["gemini"].(map[string]any); !ok {
		t.Fatalf("nexus.ext.gemini: %#v", ext["gemini"])
	}
}

func TestGetExt_missing(t *testing.T) {
	body := []byte(`{}`)
	r := GetExt(body, "anthropic", "nope")
	if r.Exists() {
		t.Fatal("expected absent")
	}
}
