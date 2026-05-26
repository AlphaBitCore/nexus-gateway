package siem

import (
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBridgeConfigDefaults(t *testing.T) {
	cfg := BridgeConfig{}
	b := NewBridge(nil, nil, cfg, testLogger())
	c := b.activeCfg.Load()
	if c == nil {
		t.Fatal("activeCfg nil after NewBridge")
	}
	if c.PollInterval.Seconds() != 30 {
		t.Errorf("expected 30s, got %v", c.PollInterval)
	}
	if c.BatchSize != 200 {
		t.Errorf("expected 200, got %d", c.BatchSize)
	}
	if c.TrafficMode != "" {
		t.Errorf("expected empty TrafficMode default, got %q", c.TrafficMode)
	}
}

func TestBridgeConfigTrafficMode(t *testing.T) {
	cfg := BridgeConfig{TrafficMode: "all"}
	b := NewBridge(nil, nil, cfg, testLogger())
	c := b.activeCfg.Load()
	if c == nil || c.TrafficMode != "all" {
		t.Errorf("expected 'all', got %+v", c)
	}

	cfg2 := BridgeConfig{TrafficMode: "security"}
	b2 := NewBridge(nil, nil, cfg2, testLogger())
	c2 := b2.activeCfg.Load()
	if c2 == nil || c2.TrafficMode != "security" {
		t.Errorf("expected 'security', got %+v", c2)
	}
}

func TestClassifyAndFilterIntegration(t *testing.T) {
	trafficEvt := Event{"hookDecision": "block", "hookReasonCode": "rate_limited"}
	adminEvt := Event{"action": "login", "entityType": "session"}
	allowedEvt := Event{"hookDecision": "allow"}

	trafficEvt["eventType"] = ClassifyTrafficEvent(trafficEvt)
	adminEvt["eventType"] = ClassifyAdminEvent(adminEvt)
	allowedEvt["eventType"] = ClassifyTrafficEvent(allowedEvt)

	if allowedEvt["eventType"] != "traffic.allowed" {
		t.Errorf("expected traffic.allowed, got %v", allowedEvt["eventType"])
	}

	all := []Event{trafficEvt, adminEvt, allowedEvt}

	filtered := FilterByEventTypes(all, []string{"session.login"})
	if len(filtered) != 1 {
		t.Errorf("expected 1, got %d", len(filtered))
	}

	allFiltered := FilterByEventTypes(all, nil)
	if len(allFiltered) != 3 {
		t.Errorf("expected 3, got %d", len(allFiltered))
	}

	trafficOnly := FilterByEventTypes(all, []string{"traffic.rate_limited", "traffic.allowed"})
	if len(trafficOnly) != 2 {
		t.Errorf("expected 2 traffic events, got %d", len(trafficOnly))
	}
}
