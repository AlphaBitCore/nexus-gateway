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

// reservedFieldIDs are wire numbers deliberately left unassigned in THIS
// tree so a concurrently developed branch can land them without a collision.
// Empty now: 105/106 (artifact_refs / compliance_coverage) and 107/108
// (end_user_id / session_id) have all landed as Fld* constants — the two
// branches merged, each keeping its own contiguous ids.
var reservedFieldIDs = map[FieldID]bool{}

// TestBinwireFieldIDsUniqueAndContiguous locks the wire-number contract: ids
// are unique (no two fields share a wire number) and fill 1..max with no gap
// except the documented reservations (an undocumented gap is a typo'd wire
// number waiting to be silently reused). Both failure modes corrupt
// cross-version decode.
func TestBinwireFieldIDsUniqueAndContiguous(t *testing.T) {
	ids := AllFieldIDs()
	seen := make(map[FieldID]bool, len(ids))
	maxID := FieldID(0)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate field id %d", id)
		}
		if reservedFieldIDs[id] {
			t.Fatalf("field id %d is reserved for a concurrent branch — allocate the next free number instead", id)
		}
		seen[id] = true
		if id > maxID {
			maxID = id
		}
	}
	for n := FieldID(1); n <= maxID; n++ {
		if !seen[n] && !reservedFieldIDs[n] {
			t.Fatalf("field id %d missing — ids must fill 1..%d except documented reservations", n, maxID)
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
