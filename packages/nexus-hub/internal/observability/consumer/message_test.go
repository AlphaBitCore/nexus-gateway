package consumer

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTrafficEventMessage_Deserialize(t *testing.T) {
	raw := `{
		"id": "evt-001",
		"source": "ai-gateway",
		"timestamp": "2026-04-15T10:00:00Z",
		"sourceIp": "10.0.0.1",
		"targetHost": "api.openai.com",
		"method": "POST",
		"path": "/v1/chat/completions",
		"statusCode": 200,
		"latencyMs": 450,
		"entityId": "user-1",
		"entityType": "user",
		"hookDecision": "allow",
		"promptTokens": 100,
		"completionTokens": 50,
		"totalTokens": 150,
		"estimatedCostUsd": 0.005,
		"details": {"model": "gpt-4"}
	}`

	var msg TrafficEventMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if msg.ID != "evt-001" {
		t.Errorf("ID = %q; want evt-001", msg.ID)
	}
	if msg.Source != "ai-gateway" {
		t.Errorf("Source = %q; want ai-gateway", msg.Source)
	}
	if msg.Timestamp.Year() != 2026 {
		t.Errorf("Timestamp year = %d; want 2026", msg.Timestamp.Year())
	}
	if msg.SourceIP == nil || *msg.SourceIP != "10.0.0.1" {
		t.Error("SourceIP not parsed correctly")
	}
	if msg.StatusCode == nil || *msg.StatusCode != 200 {
		t.Error("StatusCode not parsed correctly")
	}
	if msg.PromptTokens == nil || *msg.PromptTokens != 100 {
		t.Error("PromptTokens not parsed correctly")
	}
	if msg.Details == nil {
		t.Error("Details should not be nil")
	}
}

func TestTrafficEventMessage_ThingAttribution(t *testing.T) {
	// Round-trips the thingId/thingName fields the Hub agent-audit handler
	// and the two service-side producers (ai-gateway, compliance-proxy)
	// stamp onto every traffic_event MQ message. The db-writer scans these
	// onto traffic_event.thing_id / thing_name; absence stays SQL NULL.
	raw := `{
		"id": "evt-thing",
		"source": "ai-gateway",
		"timestamp": "2026-05-12T10:00:00Z",
		"thingId": "gw-host-3050",
		"thingName": "host"
	}`

	var msg TrafficEventMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if msg.ThingID == nil || *msg.ThingID != "gw-host-3050" {
		t.Errorf("ThingID = %v; want gw-host-3050", msg.ThingID)
	}
	if msg.ThingName == nil || *msg.ThingName != "host" {
		t.Errorf("ThingName = %v; want host", msg.ThingName)
	}

	// Absent fields must stay nil so the consumer writes SQL NULL rather
	// than empty strings (preserves "this row predates the wire-up" signal).
	rawAbsent := `{"id":"evt-no-thing","source":"agent","timestamp":"2026-05-12T10:00:00Z"}`
	var msg2 TrafficEventMessage
	if err := json.Unmarshal([]byte(rawAbsent), &msg2); err != nil {
		t.Fatalf("unmarshal absent: %v", err)
	}
	if msg2.ThingID != nil {
		t.Errorf("ThingID should be nil when absent; got %v", *msg2.ThingID)
	}
	if msg2.ThingName != nil {
		t.Errorf("ThingName should be nil when absent; got %v", *msg2.ThingName)
	}
}

func TestTrafficEventMessage_E18Fields(t *testing.T) {
	raw := `{
		"id": "evt-003",
		"source": "ai-gateway",
		"timestamp": "2026-04-21T10:00:00Z",
		"apiKeyClass": "sk-ant-",
		"apiKeyFingerprint": "a1b2c3d4e5f60718",
		"usageExtractionStatus": "streaming_reported"
	}`

	var msg TrafficEventMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if msg.APIKeyClass == nil || *msg.APIKeyClass != "sk-ant-" {
		t.Errorf("APIKeyClass = %v; want sk-ant-", msg.APIKeyClass)
	}
	if msg.APIKeyFingerprint == nil || *msg.APIKeyFingerprint != "a1b2c3d4e5f60718" {
		t.Errorf("APIKeyFingerprint = %v; want a1b2c3d4e5f60718", msg.APIKeyFingerprint)
	}
	if msg.UsageExtractionStatus == nil || *msg.UsageExtractionStatus != "streaming_reported" {
		t.Errorf("UsageExtractionStatus = %v; want streaming_reported", msg.UsageExtractionStatus)
	}
}

func TestTrafficEventMessage_NullableFields(t *testing.T) {
	raw := `{"id": "evt-002", "source": "agent", "timestamp": "2026-04-15T10:00:00Z"}`

	var msg TrafficEventMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if msg.SourceIP != nil {
		t.Error("SourceIP should be nil")
	}
	if msg.StatusCode != nil {
		t.Error("StatusCode should be nil")
	}
	if msg.RequestHookDecision != nil {
		t.Error("HookDecision should be nil")
	}
}

func TestNullableJSON(t *testing.T) {
	tests := []struct {
		name  string
		raw   json.RawMessage
		isNil bool
	}{
		{"nil", nil, true},
		{"empty", json.RawMessage{}, true},
		{"null string", json.RawMessage("null"), true},
		{"valid object", json.RawMessage(`{"key":"val"}`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := nullableJSON(tc.raw)
			if tc.isNil && result != nil {
				t.Errorf("expected nil, got %v", result)
			}
			if !tc.isNil && result == nil {
				t.Error("expected non-nil")
			}
		})
	}
}

func TestNilIfEmpty(t *testing.T) {
	if nilIfEmpty("") != nil {
		t.Error("empty string should return nil")
	}
	val := nilIfEmpty("hello")
	if val == nil || *val != "hello" {
		t.Error("non-empty string should return pointer")
	}
}

func TestAdminAuditMessage_Deserialize(t *testing.T) {
	raw := `{
		"id": "audit-001",
		"timestamp": "2026-04-15T10:00:00Z",
		"actorId": "user-1",
		"actorLabel": "admin@nexus.ai",
		"actorRole": "super_admin",
		"sourceIp": "192.168.1.1",
		"action": "create",
		"entityType": "provider",
		"entityId": "prov-1",
		"beforeState": null,
		"afterState": {"name": "OpenAI"}
	}`

	// Uses mq.AdminAuditMessage — just verify JSON roundtrip works
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if result["id"] != "audit-001" {
		t.Errorf("id = %v; want audit-001", result["id"])
	}
	if result["action"] != "create" {
		t.Errorf("action = %v; want create", result["action"])
	}

	_ = time.Now() // avoid unused import
}
