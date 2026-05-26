package hub

import (
	"encoding/json"
	"testing"
)

// TestRenameConfigHistoryResponse verifies the per-event rename: Hub's
// internal `timestamp` and `thingType` become the admin-facing `createdAt`
// and `nodeType`, while every other field (configKey, action, actorId,
// actorName, newState, newVersion, sourceIp, emergencyOverride) and the
// top-level envelope (events, total, page, pageSize) pass through intact.
func TestRenameConfigHistoryResponse(t *testing.T) {
	in := []byte(`{
		"events": [
			{
				"id": "e1",
				"timestamp": "2026-04-21T16:47:57.55Z",
				"thingType": "compliance-proxy",
				"configKey": "hooks",
				"action": "update",
				"actorId": "u-1",
				"actorName": "admin",
				"newState": null,
				"newVersion": 8,
				"sourceIp": "10.0.0.1",
				"emergencyOverride": false
			},
			{
				"id": "e2",
				"timestamp": "2026-04-21T16:47:39.289Z",
				"thingType": "ai-gateway",
				"configKey": "hooks",
				"action": "update",
				"newVersion": 7
			}
		],
		"total": 14,
		"page": 1,
		"pageSize": 20
	}`)
	out, err := RenameConfigHistoryResponse(in)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	var got struct {
		Events   []map[string]any `json:"events"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"pageSize"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	if got.Total != 14 || got.Page != 1 || got.PageSize != 20 {
		t.Errorf("envelope changed: %+v", got)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events length = %d; want 2", len(got.Events))
	}

	for i, ev := range got.Events {
		if _, leaked := ev["timestamp"]; leaked {
			t.Errorf("event[%d]: internal `timestamp` leaked", i)
		}
		if _, leaked := ev["thingType"]; leaked {
			t.Errorf("event[%d]: internal `thingType` leaked", i)
		}
	}

	// Strict per-event value checks.
	e0 := got.Events[0]
	if e0["createdAt"] != "2026-04-21T16:47:57.55Z" {
		t.Errorf("event[0].createdAt = %v", e0["createdAt"])
	}
	if e0["nodeType"] != "compliance-proxy" {
		t.Errorf("event[0].nodeType = %v", e0["nodeType"])
	}
	for _, key := range []string{"id", "configKey", "action", "actorId", "actorName", "newVersion", "sourceIp", "emergencyOverride"} {
		if _, ok := e0[key]; !ok {
			t.Errorf("event[0].%s dropped by rename", key)
		}
	}
	if e0["newVersion"].(float64) != 8 {
		t.Errorf("event[0].newVersion = %v", e0["newVersion"])
	}

	e1 := got.Events[1]
	if e1["nodeType"] != "ai-gateway" || e1["createdAt"] != "2026-04-21T16:47:39.289Z" {
		t.Errorf("event[1] rename wrong: %+v", e1)
	}
}

// TestRenameConfigHistoryResponse_EmptyEvents covers the common "no matching
// rows" case — the rename must keep the events array present (and empty)
// rather than dropping it, so the UI's table layer stays deterministic.
func TestRenameConfigHistoryResponse_EmptyEvents(t *testing.T) {
	in := []byte(`{"events":[],"total":0,"page":1,"pageSize":20}`)
	out, err := RenameConfigHistoryResponse(in)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	ev, ok := got["events"].([]any)
	if !ok || len(ev) != 0 {
		t.Errorf("events should stay as empty array; got %T %v", got["events"], got["events"])
	}
}

// TestRenameConfigHistoryResponse_InvalidJSON guards the parsing contract:
// a non-object body surfaces an error rather than being silently returned
// unchanged, so admin callers get a clean 5xx instead of a malformed passthrough.
func TestRenameConfigHistoryResponse_InvalidJSON(t *testing.T) {
	if _, err := RenameConfigHistoryResponse([]byte(`not-json`)); err == nil {
		t.Error("expected error for malformed body")
	}
}
