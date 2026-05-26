package manager

import (
	"encoding/json"
	"testing"
)

func TestHeartbeatRequest_JSON(t *testing.T) {
	req := HeartbeatRequest{
		ID:          "t-1",
		Status:      "online",
		ReportedVer: 5,
		Metadata:    map[string]any{"uptime": float64(3600)},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "status", "reportedVer", "metadata"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestHeartbeatResponse_JSON(t *testing.T) {
	resp := HeartbeatResponse{
		Ack:        true,
		DesiredVer: 7,
		Desired:    map[string]any{"routing": map[string]any{"enabled": true}},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"ack", "desiredVer", "desired"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestHeartbeatResponse_DesiredOmitEmpty(t *testing.T) {
	resp := HeartbeatResponse{
		Ack:        true,
		DesiredVer: 3,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["desired"]; ok {
		t.Error("desired should be omitted when nil")
	}
}

func TestHeartbeatResponse_Roundtrip(t *testing.T) {
	original := HeartbeatResponse{
		Ack:        true,
		DesiredVer: 10,
		Desired:    map[string]any{"routing": map[string]any{"enabled": true}},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HeartbeatResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Ack != original.Ack {
		t.Errorf("Ack = %v, want %v", got.Ack, original.Ack)
	}
	if got.DesiredVer != original.DesiredVer {
		t.Errorf("DesiredVer = %d, want %d", got.DesiredVer, original.DesiredVer)
	}
}
