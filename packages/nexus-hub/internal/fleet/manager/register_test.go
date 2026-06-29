package manager

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestRegisterRequest_JSON(t *testing.T) {
	req := RegisterRequest{
		ID:       "t-1",
		Type:     "agent",
		Version:  "1.0.0",
		Address:  "10.0.0.1:8080",
		Metadata: map[string]any{"os": "linux"},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "type", "version", "address", "metadata"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestRegisterResponse_JSON(t *testing.T) {
	resp := RegisterResponse{
		Desired: map[string]any{
			"routing": map[string]any{
				"state":   map[string]any{"enabled": true},
				"version": float64(5),
			},
		},
		DesiredVer: 5,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"desired", "desiredVer"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
	desiredAny, ok := m["desired"].(map[string]any)
	if !ok {
		t.Fatalf("desired should be object, got %T", m["desired"])
	}
	routingAny, ok := desiredAny["routing"].(map[string]any)
	if !ok {
		t.Fatalf("desired.routing should be object, got %T", desiredAny["routing"])
	}
	if _, ok := routingAny["state"]; !ok {
		t.Error("desired.routing.state missing")
	}
	if _, ok := routingAny["version"]; !ok {
		t.Error("desired.routing.version missing")
	}
}

func TestRegisterResponse_EmptyDesired(t *testing.T) {
	resp := RegisterResponse{
		Desired:    map[string]any{},
		DesiredVer: 0,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got RegisterResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Desired) != 0 {
		t.Errorf("expected empty desired, got %v", got.Desired)
	}
	if got.DesiredVer != 0 {
		t.Errorf("expected desiredVer 0, got %d", got.DesiredVer)
	}
}

func TestRegisterRequest_ZeroValue(t *testing.T) {
	var req RegisterRequest
	if req.ID != "" || req.Type != "" || req.Version != "" || req.Address != "" || req.Metadata != nil {
		t.Error("zero value should have all empty fields")
	}
}
