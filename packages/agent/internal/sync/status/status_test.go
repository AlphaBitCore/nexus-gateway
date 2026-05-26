package status

import (
	"testing"
	"time"
)

func newTestCollector(opts ...func(*Collector)) *Collector {
	c := NewCollector(CollectorConfig{
		Version:         "1.2.0",
		DeviceID:        "dev-001",
		DashboardURL:    "https://gw.example.com",
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		HeartbeatSec:    60,
		UnsyncedCountFn: func() int { return 0 },
	})
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func TestCollect_ActiveState(t *testing.T) {
	c := newTestCollector()
	snap := c.Collect()
	if snap.State != "active" {
		t.Errorf("expected active, got %s", snap.State)
	}
	if snap.StateReason != "" {
		t.Errorf("expected empty reason, got %s", snap.StateReason)
	}
	if !snap.GatewayConnected {
		t.Error("expected connected")
	}
	if snap.Agent.Version != "1.2.0" {
		t.Errorf("expected 1.2.0, got %s", snap.Agent.Version)
	}
	if snap.DashboardURL != "https://gw.example.com" {
		t.Error("dashboardURL not set")
	}
}

func TestCollect_DegradedWhenOffline(t *testing.T) {
	c := newTestCollector(func(c *Collector) {
		c.SetGatewayConnected(false)
	})
	snap := c.Collect()
	if snap.State != "degraded" {
		t.Errorf("expected degraded when offline, got %s", snap.State)
	}
	if snap.StateReason != "Gateway unreachable" {
		t.Errorf("expected reason, got %s", snap.StateReason)
	}
}

func TestCollect_DegradedWhenQueueBacklog(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 150 },
	})
	snap := c.Collect()
	if snap.State != "degraded" {
		t.Errorf("expected degraded when queue > 100, got %s", snap.State)
	}
}

func TestCollect_DegradedWhenCertExpiringSoon(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(20 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	snap := c.Collect()
	if snap.State != "degraded" {
		t.Errorf("expected degraded when cert expiring soon, got %s", snap.State)
	}
}

func TestCollect_ErrorWhenNotEnrolled(t *testing.T) {
	c := NewCollector(CollectorConfig{
		UnsyncedCountFn: func() int { return 0 },
	})
	// Override enrolled to false (NewCollector sets it to true).
	c.mu.Lock()
	c.enrolled = false
	c.mu.Unlock()

	snap := c.Collect()
	if snap.State != "error" {
		t.Errorf("expected error when not enrolled, got %s", snap.State)
	}
}

func TestCollect_AuditQueueInfo(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 42 },
	})
	c.SetLastSyncTime(time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC))

	snap := c.Collect()
	if snap.AuditQueue.UnsyncedCount != 42 {
		t.Errorf("expected 42 unsynced, got %d", snap.AuditQueue.UnsyncedCount)
	}
	if snap.AuditQueue.LastSyncTime == "" {
		t.Error("expected non-empty last sync time")
	}
}

// TestCollect_QuitAllowed_DefaultsToTrue locks the back-compat path —
// when the daemon doesn't wire a provider (every legacy / test harness
// in the tree today), the snapshot must report `quitAllowed=true` so
// the menu-bar UI continues to render Restart Agent + Quit Nexus Agent
// the way it always has.
func TestCollect_QuitAllowed_DefaultsToTrue(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
	})
	snap := c.Collect()
	if !snap.Agent.QuitAllowed {
		t.Error("expected QuitAllowed=true when no provider is wired (back-compat default)")
	}
}

// TestCollect_QuitAllowed_HonoursProvider pins the actual provider
// path: when the daemon wires a QuitAllowedFn that returns false, the
// snapshot must propagate the deny so the menu-bar UI hides the
// affordances. Mirrors the prod compliance-always-on configuration
// (agent.prod.yaml: quitAllowed: false).
func TestCollect_QuitAllowed_HonoursProvider(t *testing.T) {
	c := NewCollector(CollectorConfig{
		CertExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		UnsyncedCountFn: func() int { return 0 },
		QuitAllowedFn:   func() bool { return false },
	})
	snap := c.Collect()
	if snap.Agent.QuitAllowed {
		t.Error("expected QuitAllowed=false when provider returns false")
	}
}
