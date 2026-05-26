package traffic

import (
	"strings"
	"testing"
)

func TestCollectExtra_ReturnsUnconsumedKeys(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"model": "gpt-4o",
		"x_new_field": {"sensitive": "data"},
		"another_field": 42
	}`)
	extra := CollectExtra(body, []string{"messages", "model"})
	if len(extra) != 2 {
		t.Fatalf("len(extra)=%d want 2; extra=%v", len(extra), extra)
	}
	if x, ok := extra["x_new_field"]; !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("x_new_field=%q", x)
	}
	if a, ok := extra["another_field"]; !ok || a != "42" {
		t.Errorf("another_field=%q", a)
	}
}

func TestCollectExtra_NilOnEmpty(t *testing.T) {
	if extra := CollectExtra(nil, []string{"a"}); extra != nil {
		t.Errorf("nil body must return nil; got %v", extra)
	}
	if extra := CollectExtra([]byte{}, []string{"a"}); extra != nil {
		t.Errorf("empty body must return nil; got %v", extra)
	}
}

func TestCollectExtra_NilOnInvalidJSON(t *testing.T) {
	if extra := CollectExtra([]byte(`not json`), []string{"a"}); extra != nil {
		t.Errorf("invalid JSON must return nil; got %v", extra)
	}
}

func TestCollectExtra_NilOnNonObject(t *testing.T) {
	// Top-level JSON arrays / strings have no top-level keys.
	if extra := CollectExtra([]byte(`["x"]`), []string{}); extra != nil {
		t.Errorf("top-level array must return nil; got %v", extra)
	}
	if extra := CollectExtra([]byte(`"plain string"`), []string{}); extra != nil {
		t.Errorf("top-level string must return nil; got %v", extra)
	}
}

func TestCollectExtra_NilWhenAllConsumed(t *testing.T) {
	body := []byte(`{"a":1,"b":2}`)
	if extra := CollectExtra(body, []string{"a", "b"}); extra != nil {
		t.Errorf("all keys consumed must return nil; got %v", extra)
	}
}

func TestCollectExtra_PreservesRawJSON(t *testing.T) {
	// CollectExtra returns the raw JSON of the value so downstream
	// hooks can re-parse without losing structure.
	body := []byte(`{"x":{"nested":[1,2,3]}}`)
	extra := CollectExtra(body, nil)
	if extra["x"] != `{"nested":[1,2,3]}` {
		t.Errorf("Extra[x]=%q want raw JSON object", extra["x"])
	}
}
