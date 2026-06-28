package mq

import (
	"reflect"
	"strings"
	"testing"
)

// TestBinwireFieldRegistryNoDrift keeps the binary field-id registry in lockstep
// with the wire struct. A TrafficEventMessage field added without a registered
// FieldID would be silently dropped by AppendBinary; this fails the build/CI the
// moment the json-tagged field count diverges from the registry length, forcing
// the author to register the id (and then handle it on both encode and decode).
func TestBinwireFieldRegistryNoDrift(t *testing.T) {
	want := countJSONTaggedFields(reflect.TypeOf(TrafficEventMessage{}))
	got := len(AllFieldIDs())
	if got != want {
		t.Fatalf("field-id registry drift: %d ids registered but TrafficEventMessage has %d json-tagged fields — add/remove a Fld* id (and its encode+decode handling)", got, want)
	}
}

// TestBinwireFieldIDsUniqueAndContiguous locks the wire-number contract: ids are
// unique (no two fields share a wire number) and form a contiguous 1..N range (no
// gap from a retired id silently reused). Both would corrupt cross-version decode.
func TestBinwireFieldIDsUniqueAndContiguous(t *testing.T) {
	ids := AllFieldIDs()
	seen := make(map[FieldID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate field id %d", id)
		}
		seen[id] = true
	}
	for n := 1; n <= len(ids); n++ {
		if !seen[FieldID(n)] {
			t.Fatalf("field id %d missing — ids must be contiguous 1..%d", n, len(ids))
		}
	}
}

func countJSONTaggedFields(tp reflect.Type) int {
	n := 0
	for i := range tp.NumField() {
		tag := tp.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if strings.Split(tag, ",")[0] == "" {
			continue
		}
		n++
	}
	return n
}
