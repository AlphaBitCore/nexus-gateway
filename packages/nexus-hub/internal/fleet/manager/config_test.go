package manager

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

func TestUpdateConfigRequest_JSON(t *testing.T) {
	req := UpdateConfigRequest{
		ThingType: "agent",
		ConfigKey: "routing",
		State:     map[string]any{"enabled": true},
		Action:    "update",
		ActorID:   "user-1",
		ActorName: "Admin",
		SourceIP:  "10.0.0.1",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"thingType", "configKey", "state", "action", "actorId", "actorName", "sourceIp"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestUpdateConfigResponse_JSON(t *testing.T) {
	resp := UpdateConfigResponse{
		OK:              true,
		Version:         5,
		ThingDesiredVer: 77,
		ThingsNotified:  3,
		ThingsOnline:    10,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"ok", "version", "thingDesiredVer", "thingsNotified", "thingsOnline"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}

	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["version"].(float64) != 5 {
		t.Errorf("version = %v, want 5", m["version"])
	}
	if m["thingDesiredVer"].(float64) != 77 {
		t.Errorf("thingDesiredVer = %v, want 77", m["thingDesiredVer"])
	}
}

func TestConfigChangedMessage_JSON(t *testing.T) {
	stateBytes, _ := json.Marshal(map[string]any{"enabled": true})
	msg := ConfigChangedMessage{
		Type:       "config_changed",
		ConfigKey:  "routing",
		State:      json.RawMessage(stateBytes),
		DesiredVer: 7,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"type", "configKey", "state", "desiredVer"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestConfigChangedMessage_NoDesiredField(t *testing.T) {
	stateBytes, _ := json.Marshal("val")
	msg := ConfigChangedMessage{
		Type:       "config_changed",
		ConfigKey:  "routing",
		State:      json.RawMessage(stateBytes),
		DesiredVer: 1,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["desired"]; ok {
		t.Error("desired field must not be present in per-key delta message")
	}
}

// fakeWS implements WSPool for broadcast payload tests.
type fakeWS struct {
	broadcastFn func(thingType string, data []byte) int
}

func (f *fakeWS) Broadcast(thingType string, data []byte) int {
	return f.broadcastFn(thingType, data)
}

func (f *fakeWS) Send(_ string, _ []byte) bool { return false }
func (f *fakeWS) IsConnected(_ string) bool    { return false }

func TestBroadcastConfigChanged_Payload(t *testing.T) {
	captured := make(chan []byte, 1)
	m := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws: &fakeWS{broadcastFn: func(_ string, data []byte) int {
			captured <- data
			return 1
		}},
	}

	notified := m.broadcastConfigChanged("agent", "hooks",
		json.RawMessage(`{"enabled":true}`), 7)
	if notified != 1 {
		t.Fatalf("notified = %d; want 1", notified)
	}

	var out map[string]any
	if err := json.Unmarshal(<-captured, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["type"] != "config_changed" {
		t.Errorf("type=%v; want config_changed", out["type"])
	}
	if out["configKey"] != "hooks" {
		t.Errorf("configKey=%v; want hooks", out["configKey"])
	}
	if _, ok := out["state"]; !ok {
		t.Error("state field missing")
	}
	if _, ok := out["desired"]; ok {
		t.Error("desired field must not be set on per-key delta")
	}
}

func TestHubSignal_JSON(t *testing.T) {
	sig := HubSignal{
		Action:    "config_changed",
		SourceHub: "hub-1",
		ThingType: "agent",
		ConfigKey: "routing",
		State:     map[string]any{"enabled": true},
		Version:   3,
	}
	data, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"action", "sourceHub", "thingType", "configKey", "state", "version"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestHubSignal_Roundtrip(t *testing.T) {
	original := HubSignal{
		Action:    "config_changed",
		SourceHub: "hub-east-1",
		ThingType: "service",
		ConfigKey: "quota",
		State:     map[string]any{"maxRPM": float64(1000)},
		Version:   42,
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HubSignal
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Action != original.Action {
		t.Errorf("Action = %q, want %q", got.Action, original.Action)
	}
	if got.SourceHub != original.SourceHub {
		t.Errorf("SourceHub = %q, want %q", got.SourceHub, original.SourceHub)
	}
	if got.Version != original.Version {
		t.Errorf("Version = %d, want %d", got.Version, original.Version)
	}
}
