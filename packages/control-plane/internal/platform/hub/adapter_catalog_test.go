package hub

import (
	"encoding/json"
	"testing"
)

// TestRenameConfigCatalogResponse locks in the per-entry rename: Hub's
// `thingType` becomes the product-facing `nodeType`, `configKeys` is
// unchanged, and the `entries` envelope passes through.
func TestRenameConfigCatalogResponse(t *testing.T) {
	in := []byte(`{
		"entries": [
			{"thingType":"ai-gateway","configKeys":["credentials","hooks"]},
			{"thingType":"compliance-proxy","configKeys":["killswitch"]}
		]
	}`)
	out, err := RenameConfigCatalogResponse(in)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	var got struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries length = %d; want 2", len(got.Entries))
	}

	for i, e := range got.Entries {
		if _, leaked := e["thingType"]; leaked {
			t.Errorf("entry[%d]: internal `thingType` leaked: %+v", i, e)
		}
		if _, ok := e["nodeType"]; !ok {
			t.Errorf("entry[%d]: product `nodeType` missing: %+v", i, e)
		}
		keys, ok := e["configKeys"].([]any)
		if !ok || len(keys) == 0 {
			t.Errorf("entry[%d]: configKeys not preserved: %+v", i, e)
		}
	}

	if got.Entries[0]["nodeType"] != "ai-gateway" {
		t.Errorf("entry[0].nodeType = %v", got.Entries[0]["nodeType"])
	}
	if got.Entries[1]["nodeType"] != "compliance-proxy" {
		t.Errorf("entry[1].nodeType = %v", got.Entries[1]["nodeType"])
	}
}

// TestRenameConfigCatalogResponse_EmptyEntries guards the "no templates yet"
// case — the envelope must stay intact (entries: []) so the UI's Select can
// render a clean "All" option with no real types under it.
func TestRenameConfigCatalogResponse_EmptyEntries(t *testing.T) {
	in := []byte(`{"entries":[]}`)
	out, err := RenameConfigCatalogResponse(in)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	// With entries absent/empty the rewrite path short-circuits; result
	// must still be valid JSON containing `entries: []`.
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev, ok := got["entries"].([]any)
	if !ok || len(ev) != 0 {
		t.Errorf("entries should be empty []; got %T %v", got["entries"], got["entries"])
	}
}

// TestRenameConfigCatalogResponse_InvalidJSON asserts malformed input fails
// loudly rather than being silently returned unchanged.
func TestRenameConfigCatalogResponse_InvalidJSON(t *testing.T) {
	if _, err := RenameConfigCatalogResponse([]byte(`not-json`)); err == nil {
		t.Error("expected error for malformed body")
	}
}
