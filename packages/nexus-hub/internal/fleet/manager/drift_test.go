package manager

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// TestConfigChangedMessage_PerKeyDelta verifies that the drift-repair message
// uses the per-key delta contract (ConfigKey + State, no Desired field).
func TestConfigChangedMessage_PerKeyDelta(t *testing.T) {
	stateBytes, _ := json.Marshal(map[string]any{"enabled": true})
	msg := ConfigChangedMessage{
		Type:       "config_changed",
		ConfigKey:  "routing",
		State:      json.RawMessage(stateBytes),
		DesiredVer: 10,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["type"] != "config_changed" {
		t.Errorf("type = %q, want config_changed", out["type"])
	}
	if out["configKey"] != "routing" {
		t.Errorf("configKey = %q, want routing", out["configKey"])
	}
	if _, ok := out["state"]; !ok {
		t.Error("state field missing")
	}
	if _, ok := out["desired"]; ok {
		t.Error("desired field must not be present in per-key delta")
	}
	if out["desiredVer"].(float64) != 10 {
		t.Errorf("desiredVer = %v, want 10", out["desiredVer"])
	}
}

// TestRePushConfig_FanOut verifies that rePushConfigForThing emits exactly one
// ConfigChangedMessage per key in thing.Desired and that each message has the
// correct Type, DesiredVer, ConfigKey, and no "desired" field.
func TestRePushConfig_FanOut(t *testing.T) {
	thing := &store.Thing{
		ID:   "thing-1",
		Type: "agent",
		Desired: map[string]any{
			"hooks":  map[string]any{"enabled": true},
			"policy": map[string]any{"level": "high"},
		},
		DesiredVer: 42,
	}

	ws := &mockWSPool{
		connectedIDs: map[string]bool{"thing-1": true},
	}
	mgr := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws:     ws,
	}

	err := mgr.rePushConfigForThing(context.Background(), "agent", thing)
	if err != nil {
		t.Fatalf("rePushConfigForThing: %v", err)
	}

	ws.mu.Lock()
	calls := ws.sendCalls
	ws.mu.Unlock()

	// Exactly 2 Send calls must be made to "thing-1".
	if len(calls) != 2 {
		t.Fatalf("Send call count = %d; want 2", len(calls))
	}
	for i, c := range calls {
		if c.ThingID != "thing-1" {
			t.Errorf("call[%d].ThingID = %q; want %q", i, c.ThingID, "thing-1")
		}
	}

	// Decode each message and collect config keys.
	validKeys := map[string]bool{"hooks": true, "policy": true}
	seenKeys := make(map[string]bool, 2)
	for i, c := range calls {
		var out map[string]any
		if err := json.Unmarshal(c.Data, &out); err != nil {
			t.Fatalf("call[%d] unmarshal: %v", i, err)
		}
		if out["type"] != "config_changed" {
			t.Errorf("call[%d] type = %q; want config_changed", i, out["type"])
		}
		if out["desiredVer"].(float64) != 42 {
			t.Errorf("call[%d] desiredVer = %v; want 42", i, out["desiredVer"])
		}
		if _, ok := out["desired"]; ok {
			t.Errorf("call[%d] must not contain 'desired' field", i)
		}
		ck, _ := out["configKey"].(string)
		if !validKeys[ck] {
			t.Errorf("call[%d] unexpected configKey %q", i, ck)
		}
		seenKeys[ck] = true
	}

	// Both keys must appear exactly once.
	for k := range validKeys {
		if !seenKeys[k] {
			t.Errorf("configKey %q was never sent", k)
		}
	}
}

// TestRePushConfigKey_WSPath verifies that rePushConfigKeyForThing sends
// exactly one config_changed message for the requested key via WebSocket
// when the Thing is connected locally, carrying the per-key state and the
// Thing's current DesiredVer (no version bump, no MQ fallback).
func TestRePushConfigKey_WSPath(t *testing.T) {
	thing := &store.Thing{
		ID:   "thing-1",
		Type: "ai-gateway",
		Desired: map[string]any{
			"credentials": map[string]any{"version": 7},
			"hooks":       map[string]any{"enabled": true},
		},
		DesiredVer: 9,
	}
	ws := &mockWSPool{connectedIDs: map[string]bool{"thing-1": true}}
	mq := &mockMQProducer{}
	mgr := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws:     ws,
		mq:     mq,
		hubID:  "hub-1",
	}

	if err := mgr.rePushConfigKeyForThing(context.Background(), thing, "credentials"); err != nil {
		t.Fatalf("rePushConfigKeyForThing: %v", err)
	}

	ws.mu.Lock()
	sendCalls := append([]mockSendCall(nil), ws.sendCalls...)
	ws.mu.Unlock()
	if len(sendCalls) != 1 {
		t.Fatalf("Send call count = %d; want 1 (single-key replay)", len(sendCalls))
	}
	if sendCalls[0].ThingID != "thing-1" {
		t.Errorf("ThingID = %q; want thing-1", sendCalls[0].ThingID)
	}

	var out map[string]any
	if err := json.Unmarshal(sendCalls[0].Data, &out); err != nil {
		t.Fatalf("unmarshal WS payload: %v", err)
	}
	if out["type"] != "config_changed" {
		t.Errorf("type = %v; want config_changed", out["type"])
	}
	if out["configKey"] != "credentials" {
		t.Errorf("configKey = %v; want credentials", out["configKey"])
	}
	if out["desiredVer"].(float64) != 9 {
		t.Errorf("desiredVer = %v; want 9 (must not bump)", out["desiredVer"])
	}
	if out["force"] != true {
		t.Errorf("force = %v; want true (admin resync must bypass version gate)", out["force"])
	}
	if _, leaked := out["desired"]; leaked {
		t.Error("full 'desired' map must not be present in per-key delta")
	}

	mq.mu.Lock()
	mqCount := mq.publishCount
	mq.mu.Unlock()
	if mqCount != 0 {
		t.Errorf("MQ publishCount = %d; local WS delivery must not also publish a signal", mqCount)
	}
}

// TestRePushConfigKey_MQFallback covers the case where the Thing is connected
// to a peer Hub: the local WS pool reports IsConnected=false, so rePush must
// publish a nexus.hub.signal message carrying the per-key state + the Thing's
// type so the peer Hub can route the delivery.
func TestRePushConfigKey_MQFallback(t *testing.T) {
	thing := &store.Thing{
		ID:         "thing-remote",
		Type:       "compliance-proxy",
		Desired:    map[string]any{"killswitch": map[string]any{"engaged": false}},
		DesiredVer: 3,
	}
	ws := &mockWSPool{} // no connected IDs ⇒ IsConnected=false
	mq := &mockMQProducer{}
	mgr := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws:     ws,
		mq:     mq,
		hubID:  "hub-east-1",
	}

	if err := mgr.rePushConfigKeyForThing(context.Background(), thing, "killswitch"); err != nil {
		t.Fatalf("rePushConfigKeyForThing: %v", err)
	}

	ws.mu.Lock()
	sendCount := len(ws.sendCalls)
	ws.mu.Unlock()
	if sendCount != 0 {
		t.Errorf("WS Send count = %d; want 0 when Thing is not connected locally", sendCount)
	}

	mq.mu.Lock()
	topic := mq.lastTopic
	data := append([]byte(nil), mq.lastData...)
	count := mq.publishCount
	mq.mu.Unlock()
	if count != 1 {
		t.Fatalf("MQ publishCount = %d; want 1", count)
	}
	if topic != "nexus.hub.signal" {
		t.Errorf("MQ topic = %q; want nexus.hub.signal", topic)
	}
	// The frame rides the signed-envelope transport like every other hub
	// signal; peers decode it via VerifyAndDecodeHubSignal.
	sig, ok := VerifyAndDecodeHubSignal(data, nil)
	if !ok {
		t.Fatalf("decode hub signal envelope failed: %s", data)
	}
	if sig.Action != "config_changed" || sig.SourceHub != "hub-east-1" {
		t.Errorf("signal envelope = %+v", sig)
	}
	if sig.ThingType != "compliance-proxy" || sig.ConfigKey != "killswitch" {
		t.Errorf("signal routing fields wrong: %+v", sig)
	}
	if sig.Version != 3 {
		t.Errorf("signal version = %d; want 3", sig.Version)
	}
	// Peer-Hub path must also carry Force=true + ThingID so the other Hub
	// forwards a targeted forced replay (not a type-wide broadcast) to the
	// exact Thing the operator clicked on.
	if !sig.Force {
		t.Errorf("hub signal force = %v; want true", sig.Force)
	}
	if sig.ThingID != "thing-remote" {
		t.Errorf("hub signal thingId = %q; want thing-remote", sig.ThingID)
	}
}

// TestRePushConfigKey_MQFallback_SignedFrame verifies that the targeted-resync
// MQ fallback publishes an HMAC-signed hub-signal frame: with the signing
// secret installed, the published frame round-trips VerifyAndDecodeHubSignal
// under the same secret, and is DROPPED (ok=false) by a verifier holding a
// different secret. Peer Hubs require the HMAC and drop unsigned frames, so a
// resync published without SignHubSignal would never be delivered — and an
// actor with bare MQ access must not be able to forge a forced replay.
func TestRePushConfigKey_MQFallback_SignedFrame(t *testing.T) {
	thing := &store.Thing{
		ID:         "thing-remote",
		Type:       "agent",
		Desired:    map[string]any{"hooks": map[string]any{"enabled": true}},
		DesiredVer: 7,
	}
	secret := []byte("resync-signing-secret")
	ws := &mockWSPool{} // not connected locally ⇒ MQ fallback
	mq := &mockMQProducer{}
	mgr := &Manager{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws:           ws,
		mq:           mq,
		hubID:        "hub-east-1",
		signalSecret: secret,
	}

	if err := mgr.rePushConfigKeyForThing(context.Background(), thing, "hooks"); err != nil {
		t.Fatalf("rePushConfigKeyForThing: %v", err)
	}

	mq.mu.Lock()
	data := append([]byte(nil), mq.lastData...)
	mq.mu.Unlock()

	sig, ok := VerifyAndDecodeHubSignal(data, secret)
	if !ok {
		t.Fatalf("published resync frame did not verify under the signing secret: %s", data)
	}
	if sig.ThingID != "thing-remote" || sig.ConfigKey != "hooks" || !sig.Force || sig.Version != 7 {
		t.Errorf("decoded signal = %+v; want thingId=thing-remote configKey=hooks force=true version=7", sig)
	}

	// A peer holding a different secret must drop the frame.
	if _, ok := VerifyAndDecodeHubSignal(data, []byte("other-secret")); ok {
		t.Error("frame verified under a different secret; want drop (ok=false)")
	}
}

// TestRePushConfigKey_MissingKey verifies that asking to replay a key that is
// not present in thing.Desired surfaces ErrConfigKeyNotInDesired so the HTTP
// handler can return 404 instead of silently succeeding with no delivery.
func TestRePushConfigKey_MissingKey(t *testing.T) {
	thing := &store.Thing{
		ID:         "thing-1",
		Type:       "agent",
		Desired:    map[string]any{"routing": map[string]any{"enabled": true}},
		DesiredVer: 1,
	}
	ws := &mockWSPool{connectedIDs: map[string]bool{"thing-1": true}}
	mgr := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws:     ws,
		hubID:  "hub-1",
	}

	err := mgr.rePushConfigKeyForThing(context.Background(), thing, "credentials")
	if !errors.Is(err, ErrConfigKeyNotInDesired) {
		t.Fatalf("err = %v; want ErrConfigKeyNotInDesired", err)
	}
	ws.mu.Lock()
	sendCount := len(ws.sendCalls)
	ws.mu.Unlock()
	if sendCount != 0 {
		t.Errorf("Send count = %d; want 0 when key is missing", sendCount)
	}
}

// TestRePushConfig_EmptyDesired_NoOp verifies that rePushConfigForThing is a
// no-op (no Send calls, nil error) when thing.Desired is empty, and that the
// logger emits a warn event with key "repush_noop".
func TestRePushConfig_EmptyDesired_NoOp(t *testing.T) {
	thing := &store.Thing{
		ID:      "thing-2",
		Type:    "agent",
		Desired: map[string]any{},
	}

	ws := &mockWSPool{
		connectedIDs: map[string]bool{"thing-2": true},
	}

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	mgr := &Manager{
		logger: slog.New(handler),
		ws:     ws,
	}

	err := mgr.rePushConfigForThing(context.Background(), "agent", thing)
	if err != nil {
		t.Fatalf("expected nil error for empty Desired, got: %v", err)
	}

	ws.mu.Lock()
	sendCount := len(ws.sendCalls)
	ws.mu.Unlock()
	if sendCount != 0 {
		t.Errorf("Send call count = %d; want 0 for empty Desired", sendCount)
	}

	// Verify the warn log contains the "repush_noop" event key.
	logOutput := buf.String()
	if !bytes.Contains([]byte(logOutput), []byte("repush_noop")) {
		t.Errorf("expected warn log with event=repush_noop, got: %s", logOutput)
	}
}

// TestRePushConfigKey_WSSendFalse_FallsThroughToMQ verifies that when the
// WS pool reports IsConnected=true but Send returns false (a close-during-call
// race or a write error), the function falls through to the MQ branch instead
// of silently reporting success. Without this, an admin force-resync that
// races against a Thing reconnect would log "ws sent" and never deliver.
func TestRePushConfigKey_WSSendFalse_FallsThroughToMQ(t *testing.T) {
	thing := &store.Thing{
		ID:         "thing-racy",
		Type:       "agent",
		Desired:    map[string]any{"hooks": map[string]any{"enabled": true}},
		DesiredVer: 5,
	}
	ws := &mockWSPool{
		connectedIDs: map[string]bool{"thing-racy": true},
		sendReturn:   map[string]bool{"thing-racy": false},
	}
	mq := &mockMQProducer{}
	mgr := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws:     ws,
		mq:     mq,
		hubID:  "hub-1",
	}

	if err := mgr.rePushConfigKeyForThing(context.Background(), thing, "hooks"); err != nil {
		t.Fatalf("rePushConfigKeyForThing: %v", err)
	}

	// One WS Send call attempted; failed; one MQ publish followed.
	ws.mu.Lock()
	wsCount := len(ws.sendCalls)
	ws.mu.Unlock()
	if wsCount != 1 {
		t.Errorf("WS Send count = %d; want 1 (the failed attempt)", wsCount)
	}

	mq.mu.Lock()
	mqCount := mq.publishCount
	mq.mu.Unlock()
	if mqCount != 1 {
		t.Errorf("MQ publishCount = %d; want 1 (fall-through to MQ)", mqCount)
	}
}

// TestRePushConfigKey_NoDeliveryPath_ReturnsSentinel verifies that when the
// Thing is not WS-connected and no MQ is configured, the function returns
// ErrNoDeliveryPath. Without this sentinel, an audit-committed override
// the Thing never receives looked like success at the override-set caller.
func TestRePushConfigKey_NoDeliveryPath_ReturnsSentinel(t *testing.T) {
	thing := &store.Thing{
		ID:         "thing-isolated",
		Type:       "agent",
		Desired:    map[string]any{"hooks": map[string]any{"enabled": true}},
		DesiredVer: 1,
	}
	ws := &mockWSPool{} // not connected
	mgr := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ws:     ws,
		mq:     nil, // no MQ configured
		hubID:  "hub-1",
	}

	err := mgr.rePushConfigKeyForThing(context.Background(), thing, "hooks")
	if !errors.Is(err, ErrNoDeliveryPath) {
		t.Fatalf("err = %v; want ErrNoDeliveryPath", err)
	}
}
