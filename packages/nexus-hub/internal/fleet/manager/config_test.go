package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// fanoutCounterValue reads the config.fanout_failed_total counter for a given
// path label off the registry. Returns 0 when the series has not been
// incremented for that path.
func fanoutCounterValue(reg *opsmetrics.Registry, path string) float64 {
	for _, s := range reg.Collect() {
		if s.Name == "config.fanout_failed_total" && s.DimensionKey == "path="+path {
			return s.Value
		}
	}
	return 0
}

// newFanoutTestManager builds a Manager with a real (namespace-less) ops
// registry wired so the fanout-failure counter can be read back.
func newFanoutTestManager(t *testing.T, ws WSPool, mq *mockMQProducer) (*Manager, *opsmetrics.Registry) {
	t.Helper()
	reg := opsmetrics.NewRegistryWithNamespace(prometheus.NewRegistry(), "")
	m := &Manager{
		ws:     ws,
		hubID:  "hub-test",
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mq != nil {
		m.mq = mq
	}
	m.SetFanoutMetrics(reg)
	return m, reg
}

// TestManager_FanoutMetrics asserts the F-0114 named failure mode: every
// post-commit fan-out failure (WS broadcast skip / marshal, MQ marshal /
// publish) increments config_fanout_failed_total{path} instead of returning
// silently — so a NATS outage stranding peer-Hub Things is observable
// immediately, not only via the lagging drift gauge 60s later.
func TestManager_FanoutMetrics(t *testing.T) {
	t.Run("ws unavailable increments path=ws", func(t *testing.T) {
		m, reg := newFanoutTestManager(t, nil, nil) // ws == nil
		if n := m.broadcastConfigChanged("agent", "routing", map[string]any{"e": true}, 1); n != 0 {
			t.Errorf("notified = %d, want 0 when ws nil", n)
		}
		if v := fanoutCounterValue(reg, "ws"); v != 1 {
			t.Errorf("path=ws counter = %v, want 1", v)
		}
	})
	t.Run("broadcast state marshal failure increments path=ws", func(t *testing.T) {
		m, reg := newFanoutTestManager(t, &mockWSPool{}, nil)
		// A channel value cannot be JSON-marshalled → the state marshal branch.
		m.broadcastConfigChanged("agent", "routing", map[string]any{"bad": make(chan int)}, 1)
		if v := fanoutCounterValue(reg, "ws"); v != 1 {
			t.Errorf("path=ws counter = %v, want 1", v)
		}
	})
	t.Run("hub signal marshal failure increments path=nats", func(t *testing.T) {
		m, reg := newFanoutTestManager(t, &mockWSPool{}, &mockMQProducer{})
		// State with an unmarshalable chan makes json.Marshal(HubSignal) fail.
		m.publishHubSignal(context.Background(), "agent", "routing", make(chan int), 1)
		if v := fanoutCounterValue(reg, "nats"); v != 1 {
			t.Errorf("path=nats counter = %v, want 1", v)
		}
	})
	t.Run("hub signal publish failure increments path=nats", func(t *testing.T) {
		m, reg := newFanoutTestManager(t, &mockWSPool{}, &mockMQProducer{publishErr: errors.New("nats down")})
		m.publishHubSignal(context.Background(), "agent", "routing", map[string]any{"e": true}, 1)
		if v := fanoutCounterValue(reg, "nats"); v != 1 {
			t.Errorf("path=nats counter = %v, want 1", v)
		}
	})
	t.Run("nil registry: incFanoutFailed is a no-op", func(t *testing.T) {
		m := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		// Must not panic with no registry wired.
		m.incFanoutFailed("ws")
		m.incFanoutFailed("nats")
	})
	t.Run("SetFanoutMetrics tolerates nil registry", func(t *testing.T) {
		m := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		m.SetFanoutMetrics(nil)
		if m.fanoutFailed != nil {
			t.Error("fanoutFailed should stay nil when registry is nil")
		}
	})
}

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

// TestVerifyAndDecodeHubSignal_RejectsMalformedFrames: peers must DROP (not
// panic on, not partially apply) every malformed nexus.hub.signal shape —
// garbage bytes, an envelope without payload, a forged/missing MAC under a
// configured secret, and a signed envelope whose inner payload is not a
// HubSignal.
func TestVerifyAndDecodeHubSignal_RejectsMalformedFrames(t *testing.T) {
	secret := []byte("inter-hub-secret")
	signedGarbagePayload, err := json.Marshal(hubSignalEnvelope{
		Sig:     hubSignalMAC([]byte(`"not-an-object"`), secret),
		Payload: json.RawMessage(`"not-an-object"`),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	unsigned, err := SignHubSignal(HubSignal{Action: "config_changed"}, nil)
	if err != nil {
		t.Fatalf("SignHubSignal: %v", err)
	}

	cases := []struct {
		name   string
		data   []byte
		secret []byte
	}{
		{"garbage bytes", []byte("{{{{"), nil},
		{"envelope without payload", []byte(`{"sig":"abc"}`), nil},
		{"unsigned frame under configured secret", unsigned, secret},
		{"signed envelope with non-HubSignal payload", signedGarbagePayload, secret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := VerifyAndDecodeHubSignal(tc.data, tc.secret); ok {
				t.Fatalf("frame %q must be dropped, was accepted", tc.name)
			}
		})
	}
}

// TestSetSignalSecret_SignsSubsequentFrames: installing the inter-Hub secret
// via the wiring setter must flip published frames from unsigned-accepted to
// HMAC-verified — the same frame fails verification under a different secret.
func TestSetSignalSecret_SignsSubsequentFrames(t *testing.T) {
	m := &Manager{}
	m.SetSignalSecret([]byte("wired-secret"))

	data, err := SignHubSignal(HubSignal{Action: "config_changed", ConfigKey: "k"}, m.signalSecret)
	if err != nil {
		t.Fatalf("SignHubSignal: %v", err)
	}
	if _, ok := VerifyAndDecodeHubSignal(data, []byte("wired-secret")); !ok {
		t.Fatal("frame signed with the installed secret must verify under that secret")
	}
	if _, ok := VerifyAndDecodeHubSignal(data, []byte("other-secret")); ok {
		t.Fatal("frame must NOT verify under a different secret")
	}
}
