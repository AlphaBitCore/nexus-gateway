package httperr

import "testing"

// TestErrJSONEnvelopeShape asserts the exact nested envelope shape that admin
// handlers and the Control Plane UI depend on: a top-level "error" object
// carrying message/type/code with the supplied values.
func TestErrJSONEnvelopeShape(t *testing.T) {
	got := ErrJSON("boom", "validation_error", "invalid_field")

	outer, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level \"error\" object, got %T", got["error"])
	}
	if len(got) != 1 {
		t.Fatalf("envelope must contain only the \"error\" key, got %d keys: %v", len(got), got)
	}

	for field, want := range map[string]string{
		"message": "boom",
		"type":    "validation_error",
		"code":    "invalid_field",
	} {
		if outer[field] != want {
			t.Errorf("error.%s = %v, want %q", field, outer[field], want)
		}
	}
	if len(outer) != 3 {
		t.Fatalf("error object must contain exactly message/type/code, got %d keys: %v", len(outer), outer)
	}
}
