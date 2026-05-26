//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/testharness"
)

// TestHandleBreakGlassReport_UpsertsTemplateAndInsertsEvent covers the happy
// path for break-glass reconciliation:
//
//  1. Seed thing_config_template with a baseline admin state (version 3).
//  2. A break-glass shadow_report arrives with reportedVer=4 (> 3).
//  3. Hub adopts the reported state at version 4 and emits an audit event
//     with emergency_override=true and actor_id="break-glass:<tokenID>".
func TestHandleBreakGlassReport_UpsertsTemplateAndInsertsEvent(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hub := testharness.NewForTest(t)
	st := hub.Store()
	mgr := hub.Mgr()

	const (
		thingID   = "proxy-bg-upsert"
		thingType = "compliance-proxy"
		configKey = "killswitch"
		tokenID   = "a1b2c3d4"
	)

	// --- Fixture setup ---
	// Create the Thing (required so GetThing succeeds in handleBreakGlassReport).
	if err := st.UpsertThingEnrollment(ctx, store.UpsertThingParams{
		ID:           thingID,
		Type:         thingType,
		Name:         thingID,
		AuthType:     "bearer",
		ConnProtocol: "http",
		Status:       "online",
	}); err != nil {
		t.Fatalf("UpsertThingEnrollment: %v", err)
	}

	// Bring the baseline template to version 3 via three admin upserts.
	for i := 0; i < 3; i++ {
		tx, err := st.Pool().Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := st.UpsertConfigTemplate(ctx, tx, thingType, configKey,
			map[string]any{"enabled": false}, "admin:alice"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("UpsertConfigTemplate: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanCtx := context.Background()
		pool := st.Pool()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM config_change_event WHERE thing_type=$1 AND config_key=$2`, thingType, configKey)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM thing_config_template WHERE type=$1 AND config_key=$2`, thingType, configKey)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM thing WHERE id=$1`, thingID)
	})

	// --- Exercise ---
	req := manager.ShadowReportRequest{
		ID:           thingID,
		Reported:     map[string]any{configKey: map[string]any{"enabled": true}},
		ReportedVer:  4,
		KeyVersions:  map[string]int64{configKey: 4},
		Reason:       "break_glass",
		SourceIP:     "10.0.0.7",
		ActorTokenID: tokenID,
	}
	if err := mgr.HandleShadowReport(ctx, req); err != nil {
		t.Fatalf("HandleShadowReport: %v", err)
	}

	// --- Assertions ---
	tpl, err := st.GetConfigTemplate(ctx, thingType, configKey)
	if err != nil {
		t.Fatalf("GetConfigTemplate: %v", err)
	}
	if tpl.Version != 4 {
		t.Fatalf("template version = %d, want 4", tpl.Version)
	}

	events, err := st.ListConfigHistory(ctx, store.ListConfigHistoryParams{
		ThingType: thingType,
		ConfigKey: configKey,
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("ListConfigHistory: %v", err)
	}
	var found bool
	for _, e := range events.Events {
		if e.EmergencyOverride && e.ActorID == "break-glass:"+tokenID && e.NewVersion == 4 {
			found = true
			if e.SourceIP != "10.0.0.7" {
				t.Errorf("source_ip = %q, want 10.0.0.7", e.SourceIP)
			}
			if e.Action != "emergency_override" {
				t.Errorf("action = %q, want emergency_override", e.Action)
			}
			break
		}
	}
	if !found {
		t.Fatalf("no emergency_override=true event with actor=break-glass:%s and new_version=4; got %+v",
			tokenID, events.Events)
	}
}

// TestHandleBreakGlassReport_SkipsWhenReportedLessThanCurrent verifies that a
// stale break-glass report (admin already wrote a higher version) neither
// downgrades the template nor emits an audit event.
func TestHandleBreakGlassReport_SkipsWhenReportedLessThanCurrent(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hub := testharness.NewForTest(t)
	st := hub.Store()
	mgr := hub.Mgr()

	const (
		thingID   = "proxy-bg-stale"
		thingType = "compliance-proxy"
		configKey = "killswitch"
		tokenID   = "a1b2c3d4"
	)

	if err := st.UpsertThingEnrollment(ctx, store.UpsertThingParams{
		ID:           thingID,
		Type:         thingType,
		Name:         thingID,
		AuthType:     "bearer",
		ConnProtocol: "http",
		Status:       "online",
	}); err != nil {
		t.Fatalf("UpsertThingEnrollment: %v", err)
	}

	// Drive the template to version 10.
	for i := 0; i < 10; i++ {
		tx, err := st.Pool().Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := st.UpsertConfigTemplate(ctx, tx, thingType, configKey,
			map[string]any{"enabled": false}, "admin:alice"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("UpsertConfigTemplate: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanCtx := context.Background()
		pool := st.Pool()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM config_change_event WHERE thing_type=$1 AND config_key=$2`, thingType, configKey)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM thing_config_template WHERE type=$1 AND config_key=$2`, thingType, configKey)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM thing WHERE id=$1`, thingID)
	})

	// Break-glass report carrying a stale version.
	req := manager.ShadowReportRequest{
		ID:           thingID,
		Reported:     map[string]any{configKey: map[string]any{"enabled": true}},
		ReportedVer:  5,
		KeyVersions:  map[string]int64{configKey: 5},
		Reason:       "break_glass",
		ActorTokenID: tokenID,
	}
	if err := mgr.HandleShadowReport(ctx, req); err != nil {
		t.Fatalf("HandleShadowReport: %v", err)
	}

	tpl, err := st.GetConfigTemplate(ctx, thingType, configKey)
	if err != nil {
		t.Fatalf("GetConfigTemplate: %v", err)
	}
	if tpl.Version != 10 {
		t.Fatalf("template version = %d, want 10 (must not downgrade)", tpl.Version)
	}

	events, err := st.ListConfigHistory(ctx, store.ListConfigHistoryParams{
		ThingType: thingType,
		ConfigKey: configKey,
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("ListConfigHistory: %v", err)
	}
	for _, e := range events.Events {
		if e.EmergencyOverride {
			t.Errorf("stale break-glass report must not emit an emergency_override event; got %+v", e)
		}
	}
}
