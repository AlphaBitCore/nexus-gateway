package rules

import (
	"strings"
	"testing"
)

// White-box test (same-package) because mustJSON is unexported. It is
// called at package init to build BuiltinRules — a refactor that
// swallows the marshal error would silently produce malformed
// Params/ParamsSchema blobs, which is exactly the failure-mode the
// panic is meant to surface at startup.

func TestMustJSON_PanicsOnUnmarshalable(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("mustJSON should panic on unmarshalable value")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value should be string, got %T", r)
		}
		if !strings.HasPrefix(msg, "rules: mustJSON:") {
			t.Errorf("panic message wrong prefix: %q", msg)
		}
	}()
	// chan values cannot be marshaled by encoding/json.
	_ = mustJSON(make(chan int))
}

func TestMustJSON_RoundTripsValidValue(t *testing.T) {
	// Happy path coverage too.
	got := mustJSON(map[string]int{"a": 1})
	if string(got) != `{"a":1}` {
		t.Errorf("got %q", string(got))
	}
}
