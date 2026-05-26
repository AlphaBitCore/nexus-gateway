package audit

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
)

func TestSIEMBridge_Identity(t *testing.T) {
	bridge := siem.NewBridge(nil, nil, siem.BridgeConfig{PollInterval: 45 * time.Second}, testLogger())
	j := NewSIEMBridge(bridge, testLogger())
	if j.ID() != "siem-bridge" {
		t.Errorf("ID = %q, want siem-bridge", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 45*time.Second {
		t.Errorf("Interval = %v, want 45s", j.Interval())
	}
}

func TestSIEMBridge_IntervalFromBridgeDefault(t *testing.T) {
	bridge := siem.NewBridge(nil, nil, siem.BridgeConfig{}, testLogger())
	j := NewSIEMBridge(bridge, testLogger())
	if j.Interval() != 30*time.Second {
		t.Errorf("Interval = %v, want 30s default from bridge", j.Interval())
	}
}

// Note: TestSIEMBridge_Run is omitted because siem.Bridge.Poll requires a
// concrete *pgxpool.Pool (no interface seam) and panics with nil pool even
// when Enabled=false (Reload is called unconditionally). Coverage of the Run
// method itself (one statement) is counted as untestable without a real DB —
// this is the only line left uncovered in the siem_bridge.go file.
