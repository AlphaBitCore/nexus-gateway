package canonicalext

import (
	"testing"
)

func TestGet_AbsentReturnsEmptyResult(t *testing.T) {
	r := Get([]byte(`{"foo":1}`), "anthropic", "topK")
	if r.Exists() {
		t.Errorf("absent key should yield empty Result; got %+v", r)
	}
}

func TestGet_ReadsAtCanonicalPath(t *testing.T) {
	body := []byte(`{"nexus":{"ext":{"anthropic":{"topK":42}}}}`)
	r := Get(body, "anthropic", "topK")
	if !r.Exists() {
		t.Fatal("expected Result.Exists()")
	}
	if r.Int() != 42 {
		t.Errorf("got %v, want 42", r.Int())
	}
}

func TestGet_DifferentProvidersIsolated(t *testing.T) {
	// The provider segment scopes the path — anthropic.topK must not
	// match a gemini.topK lookup or vice versa.
	body := []byte(`{"nexus":{"ext":{"anthropic":{"topK":1},"gemini":{"topK":2}}}}`)
	if r := Get(body, "anthropic", "topK"); r.Int() != 1 {
		t.Errorf("anthropic.topK: got %v, want 1", r.Int())
	}
	if r := Get(body, "gemini", "topK"); r.Int() != 2 {
		t.Errorf("gemini.topK: got %v, want 2", r.Int())
	}
}

func TestSet_WritesAtCanonicalPath(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	got, err := Set(body, "anthropic", "topK", 5)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Reading back via Get must round-trip.
	if v := Get(got, "anthropic", "topK").Int(); v != 5 {
		t.Errorf("roundtrip: got %v, want 5", v)
	}
}

func TestSet_OverwritesExisting(t *testing.T) {
	body := []byte(`{"nexus":{"ext":{"anthropic":{"topK":1}}}}`)
	got, err := Set(body, "anthropic", "topK", 99)
	if err != nil {
		t.Fatal(err)
	}
	if v := Get(got, "anthropic", "topK").Int(); v != 99 {
		t.Errorf("overwrite: got %v, want 99", v)
	}
}

func TestSet_PreservesUnrelatedKeys(t *testing.T) {
	body := []byte(`{"model":"x","temperature":0.5}`)
	got, err := Set(body, "anthropic", "topK", 5)
	if err != nil {
		t.Fatal(err)
	}
	// Untouched keys must remain.
	for _, want := range []string{`"model":"x"`, `"temperature":0.5`} {
		if !contains(string(got), want) {
			t.Errorf("Set wiped unrelated key; missing %q in %s", want, got)
		}
	}
}

func TestSet_NestedStructValue(t *testing.T) {
	// Provider extension values are typically objects (per-tool params,
	// reasoning configs, etc). The roundtrip must preserve nested shape.
	body := []byte(`{}`)
	val := map[string]any{"reasoning": map[string]any{"effort": "high"}}
	got, err := Set(body, "openai", "advanced", val)
	if err != nil {
		t.Fatal(err)
	}
	r := Get(got, "openai", "advanced")
	if r.Get("reasoning.effort").Str != "high" {
		t.Errorf("nested roundtrip failed: %s", got)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
