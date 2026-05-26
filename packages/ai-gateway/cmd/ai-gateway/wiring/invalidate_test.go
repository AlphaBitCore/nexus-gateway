package wiring

import (
	"encoding/json"
	"testing"
)

func TestParseInvalidateIDs_empty(t *testing.T) {
	if got := ParseInvalidateIDs(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %v", got)
	}
	if got := ParseInvalidateIDs([]byte{}); got != nil {
		t.Fatalf("expected nil for empty slice, got %v", got)
	}
}

func TestParseInvalidateIDs_malformedJSON(t *testing.T) {
	if got := ParseInvalidateIDs([]byte("{not json")); got != nil {
		t.Fatalf("expected nil for malformed JSON, got %v", got)
	}
}

func TestParseInvalidateIDs_wrongOp(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"op": "reload", "ids": []string{"a", "b"}})
	if got := ParseInvalidateIDs(raw); got != nil {
		t.Fatalf("expected nil for op=reload, got %v", got)
	}
}

func TestParseInvalidateIDs_missingOp(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"ids": []string{"a"}})
	if got := ParseInvalidateIDs(raw); got != nil {
		t.Fatalf("expected nil when op field absent, got %v", got)
	}
}

func TestParseInvalidateIDs_validPayload(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"op": "invalidate", "ids": []string{"id1", "id2"}})
	got := ParseInvalidateIDs(raw)
	if len(got) != 2 || got[0] != "id1" || got[1] != "id2" {
		t.Fatalf("expected [id1 id2], got %v", got)
	}
}

func TestParseInvalidateIDs_emptyIDs(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"op": "invalidate", "ids": []string{}})
	got := ParseInvalidateIDs(raw)
	// returns an empty (non-nil) slice; callers treat len==0 as full-purge
	if got == nil {
		t.Fatal("expected non-nil empty slice for valid invalidate with no ids")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty ids, got %v", got)
	}
}

func TestParseInvalidateIDs_snapshotSignal(t *testing.T) {
	// Simulate the snapshot-reload payload shape that the configdispatch
	// comment says is "arbitrary JSON" — should still return nil IDs.
	raw := []byte(`{"type":"snapshot","sequence":42}`)
	if got := ParseInvalidateIDs(raw); got != nil {
		t.Fatalf("expected nil for snapshot payload, got %v", got)
	}
}
