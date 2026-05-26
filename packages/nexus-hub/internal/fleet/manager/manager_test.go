package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNew_NilDeps(t *testing.T) {
	logger := slog.Default()
	mgr := New(nil, nil, nil, nil, "hub-test", logger)
	if mgr == nil {
		t.Fatal("New should return non-nil Manager with nil deps")
	}
	if mgr.Store() != nil {
		t.Error("Store() should be nil when constructed with nil store")
	}
	if mgr.hubID != "hub-test" {
		t.Errorf("hubID = %q, want %q", mgr.hubID, "hub-test")
	}
}

func TestNew_HubID(t *testing.T) {
	logger := slog.Default()
	tests := []struct {
		hubID string
	}{
		{""},
		{"hub-1"},
		{"us-east-1-hub"},
	}
	for _, tt := range tests {
		mgr := New(nil, nil, nil, nil, tt.hubID, logger)
		if mgr.hubID != tt.hubID {
			t.Errorf("hubID = %q, want %q", mgr.hubID, tt.hubID)
		}
	}
}

// --- nil-safe best-effort methods ---

func TestBroadcastConfigChanged_NilWS(t *testing.T) {
	logger := slog.Default()
	mgr := New(nil, nil, nil, nil, "hub-1", logger)
	notified := mgr.broadcastConfigChanged("agent", "routing", map[string]any{"enabled": true}, 1)
	if notified != 0 {
		t.Errorf("expected 0 notified with nil WS, got %d", notified)
	}
}

func TestBroadcastConfigChanged_WithMockWS(t *testing.T) {
	logger := slog.Default()
	ws := &mockWSPool{broadcastCount: 5}
	mgr := New(nil, nil, nil, ws, "hub-1", logger)
	notified := mgr.broadcastConfigChanged("agent", "routing", map[string]any{"enabled": true}, 1)
	if notified != 5 {
		t.Errorf("expected 5 notified, got %d", notified)
	}
	if ws.lastBroadcastType != "agent" {
		t.Errorf("lastBroadcastType = %q, want %q", ws.lastBroadcastType, "agent")
	}
}

func TestPublishHubSignal_NilMQ(t *testing.T) {
	logger := slog.Default()
	mgr := New(nil, nil, nil, nil, "hub-1", logger)
	// Should not panic with nil MQ
	mgr.publishHubSignal(context.Background(), "agent", "routing", map[string]any{"enabled": true}, 1)
}

func TestPublishHubSignal_WithMockMQ(t *testing.T) {
	logger := slog.Default()
	mq := &mockMQProducer{}
	mgr := New(nil, nil, mq, nil, "hub-1", logger)
	mgr.publishHubSignal(context.Background(), "agent", "routing", map[string]any{"enabled": true}, 5)
	if mq.publishCount != 1 {
		t.Errorf("expected 1 publish call, got %d", mq.publishCount)
	}
	if mq.lastTopic != "nexus.hub.signal" {
		t.Errorf("topic = %q, want %q", mq.lastTopic, "nexus.hub.signal")
	}
	var sig HubSignal
	if err := json.Unmarshal(mq.lastData, &sig); err != nil {
		t.Fatalf("unmarshal signal: %v", err)
	}
	if sig.Action != "config_changed" {
		t.Errorf("action = %q, want %q", sig.Action, "config_changed")
	}
	if sig.SourceHub != "hub-1" {
		t.Errorf("sourceHub = %q, want %q", sig.SourceHub, "hub-1")
	}
	if sig.Version != 5 {
		t.Errorf("version = %d, want %d", sig.Version, 5)
	}
}

func TestCacheDesiredKey_NilRedis(t *testing.T) {
	logger := slog.Default()
	mgr := New(nil, nil, nil, nil, "hub-1", logger)
	// Should not panic with nil Redis
	mgr.cacheDesiredKey(context.Background(), "agent", "routing", map[string]any{"enabled": true})
}

func TestCacheDesired_NilRedis(t *testing.T) {
	logger := slog.Default()
	mgr := New(nil, nil, nil, nil, "hub-1", logger)
	mgr.cacheDesired(context.Background(), "agent", map[string]any{"routing": map[string]any{"enabled": true}})
}

func TestCacheShadow_NilRedis(t *testing.T) {
	logger := slog.Default()
	mgr := New(nil, nil, nil, nil, "hub-1", logger)
	mgr.cacheShadow(context.Background(), "thing-1", map[string]any{"routing": map[string]any{"enabled": true}})
}
