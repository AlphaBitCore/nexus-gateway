package authstore

import (
	"testing"
)

// TestDecodeJSONB covers the helper used to unmarshal JSONB columns.

func TestDecodeJSONB_Nil(t *testing.T) {
	// nil/empty raw → no-op, target untouched.
	var v map[string]any
	if err := decodeJSONB(nil, &v, "test_col"); err != nil {
		t.Fatalf("decodeJSONB(nil): %v", err)
	}
	if v != nil {
		t.Error("decodeJSONB(nil) must leave target nil")
	}
}

func TestDecodeJSONB_Empty(t *testing.T) {
	var v map[string]any
	if err := decodeJSONB([]byte{}, &v, "test_col"); err != nil {
		t.Fatalf("decodeJSONB(empty): %v", err)
	}
	if v != nil {
		t.Error("decodeJSONB(empty) must leave target nil")
	}
}

func TestDecodeJSONB_Valid(t *testing.T) {
	raw := []byte(`{"key":"value"}`)
	var v map[string]any
	if err := decodeJSONB(raw, &v, "test_col"); err != nil {
		t.Fatalf("decodeJSONB(valid): %v", err)
	}
	if v["key"] != "value" {
		t.Errorf("decoded %v; want key=value", v)
	}
}

func TestDecodeJSONB_Invalid(t *testing.T) {
	raw := []byte(`{invalid json`)
	var v map[string]any
	err := decodeJSONB(raw, &v, "test_col")
	if err == nil {
		t.Error("decodeJSONB(invalid JSON) must return error")
	}
}
