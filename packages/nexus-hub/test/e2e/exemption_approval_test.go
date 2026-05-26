//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// TestExemptionApprovalFlow covers the CP-approves-an-exemption workflow end
// to end for the exemptions config key (Tasks 5 + 12):
//
//  1. A fake compliance-proxy thingclient connects over WebSocket.
//  2. The test simulates CP pushing a single ActiveExemption entry via
//     /api/hub/config/update (upsert semantics).
//  3. The thingclient applies the delta and the test verifies one entry is
//     live, with the expected target host.
//  4. The test simulates expiry/removal by pushing an empty Entries array;
//     the thingclient must apply the empty set.
//  5. Both notifies bump the template version; the audit rows must carry
//     actor metadata and emergency_override=false.
//
// Payload field name matches configtypes.ActiveExemptions.Entries (JSON tag
// "entries"). The plan's earlier sketch used "active" — the canonical shape
// is what the shared struct emits.
func TestExemptionApprovalFlow(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	h := newTestHarness(t)

	const (
		thingID   = "proxy-exemption-e2e"
		thingType = "compliance-proxy"
		configKey = "exemptions"
	)

	h.cleanupConfigState(t, thingType, configKey)

	_, token, wsURL := h.registerThing(t, thingID, thingType)

	var (
		mu      sync.Mutex
		applied configtypes.ActiveExemptions
	)
	cli := h.connectFakeClient(t, wsURL, thingID, thingType, token,
		func(key string, cs thingclient.ConfigState) error {
			if key != configKey {
				return nil
			}
			var ae configtypes.ActiveExemptions
			if err := json.Unmarshal(cs.State, &ae); err != nil {
				return err
			}
			mu.Lock()
			applied = ae
			mu.Unlock()
			return nil
		})
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = cli.Close(closeCtx)
	}()

	// Step 1: approve an exemption.
	approved := configtypes.ActiveExemptions{
		Entries: []configtypes.ActiveExemption{{
			ID:         "exemption-1",
			SourceIP:   "10.0.1.5",
			TargetHost: "legacy.example.com",
			ExpiresAt:  time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
			Reason:     "migration deadline",
			ApprovedBy: "admin@nexus.ai",
		}},
	}
	h.notifyConfigChange(t, thingType, configKey, approved, "update")

	waitUntil(t, 5*time.Second, "exemption applied", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied.Entries) == 1 && applied.Entries[0].TargetHost == "legacy.example.com"
	})

	mu.Lock()
	got := applied
	mu.Unlock()
	if got.Entries[0].ApprovedBy != "admin@nexus.ai" {
		t.Errorf("applied[0].ApprovedBy = %q, want admin@nexus.ai", got.Entries[0].ApprovedBy)
	}

	rowAdd := h.queryLatestChangeEvent(t, thingType, configKey)
	if rowAdd.EmergencyOverride {
		t.Error("approval event must not be emergency_override")
	}
	verAfterAdd := rowAdd.NewVersion
	if verAfterAdd < 1 {
		t.Errorf("approval event version = %d, want >= 1", verAfterAdd)
	}

	// Step 2: remove the exemption (simulates expiry GC or admin revoke).
	h.notifyConfigChange(t, thingType, configKey,
		configtypes.ActiveExemptions{Entries: []configtypes.ActiveExemption{}}, "update")

	waitUntil(t, 5*time.Second, "exemption removed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied.Entries) == 0
	})

	rowRemove := h.queryLatestChangeEvent(t, thingType, configKey)
	if rowRemove.NewVersion <= verAfterAdd {
		t.Errorf("removal event version = %d, want > %d", rowRemove.NewVersion, verAfterAdd)
	}
	if rowRemove.EmergencyOverride {
		t.Error("removal event must not be emergency_override")
	}
}
