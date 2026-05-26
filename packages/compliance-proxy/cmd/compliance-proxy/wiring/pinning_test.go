package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
)

func TestInitPinningTracker_NoExemptions(t *testing.T) {
	cfg := &config.Config{}
	tracker := InitPinningTracker(cfg)
	if tracker == nil {
		t.Fatal("expected non-nil pinning tracker")
	}
}

func TestInitPinningTracker_WithStaticExemptions(t *testing.T) {
	cfg := &config.Config{}
	cfg.Audit.Pinning.Exemptions = []config.PinningExemption{
		{Host: "internal.example.com", Reason: "pinned cert"},
		{Host: "legacy.example.com", Reason: "old cert chain"},
	}
	tracker := InitPinningTracker(cfg)
	if tracker == nil {
		t.Fatal("expected non-nil pinning tracker")
	}
}

func TestInitPinningTracker_WithAutoExemptEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Audit.Pinning.AutoExempt.Enabled = true
	cfg.Audit.Pinning.AutoExempt.FailureThreshold = 5
	cfg.Audit.Pinning.AutoExempt.WindowSeconds = 60
	cfg.Audit.Pinning.AutoExempt.ExemptionDurationSeconds = 3600
	tracker := InitPinningTracker(cfg)
	if tracker == nil {
		t.Fatal("expected non-nil pinning tracker when auto-exempt is on")
	}
}
