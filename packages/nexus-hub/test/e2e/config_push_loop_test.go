//go:build e2e

// Package e2e contains end-to-end regression tests for the Nexus Hub.
// These tests require a live PostgreSQL database (set DATABASE_URL) and are
// gated behind both the e2e build tag and the RUN_E2E environment variable to
// prevent accidental execution in normal CI runs.
//
// Run with:
//
//	RUN_E2E=1 DATABASE_URL=postgres://... \
//	  go test ./packages/nexus-hub/test/e2e/... \
//	  -race -count=1 -v -tags=e2e -run TestConfigPushLoop_EndToEnd
package e2e

import (
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/testharness"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// TestConfigPushLoop_EndToEnd exercises the complete config-push loop:
//
//  1. Hub receives UpdateConfig (desired_ver bumped, thing.desired updated) — C1/C2 path.
//  2. Hub broadcasts a per-key config_changed delta over WebSocket — C1 hub broadcast.
//  3. thingclient merges the delta into its desiredCache and fires OnConfigChanged — C1 client merge.
//  4. Client sends a shadow_report back with the applied version — closure of the loop.
//  5. Hub persists reported_ver == desired_ver — C4 verification.
func TestConfigPushLoop_EndToEnd(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// --- Wire Hub harness ---
	hub := testharness.NewForTest(t)
	ts := httptest.NewServer(hub.Handler())
	defer ts.Close()

	wsURL := "ws" + ts.URL[len("http"):] + "/ws?id=agent-e2e&type=agent"
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Pre-create the Thing and obtain a device token.
	token := hub.IssueEnrollmentToken(t, "agent-e2e")

	// --- Wire thingclient ---
	applied := make(chan map[string]thingclient.ConfigState, 4)

	// Use a fresh prometheus.Registry so this test plays nicely when the
	// full e2e suite runs in one process alongside other tests that spin up
	// their own thingclient instances (promauto + default registerer panics
	// on duplicate registration).
	client, err := thingclient.New(thingclient.Config{
		HubURL:            wsURL,
		HubHTTPURL:        ts.URL,
		ThingType:         "agent",
		ThingID:           "agent-e2e",
		Token:             token,
		Logger:            logger,
		MetricsRegisterer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("new thingclient: %v", err)
	}

	client.OnConfigChanged(func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		// Record the desired map for assertion; return it as-is to signal
		// full application (reported == desired).
		cp := make(map[string]thingclient.ConfigState, len(desired))
		for k, v := range desired {
			cp[k] = v
		}
		select {
		case applied <- cp:
		default:
		}
		return desired, nil
	})

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer client.Close(ctx) //nolint:errcheck

	// Wait for the WebSocket handshake to complete.
	waitForConnect(t, client)

	// --- Push a config update ---
	_, err = hub.Mgr().UpdateConfig(ctx, manager.UpdateConfigRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
		State:     json.RawMessage(`{"enabled":true}`),
		Action:    "update",
		ActorID:   testharness.E2EActorID,
		ActorName: "e2e-harness",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	// --- Assert OnConfigChanged was called with hooks key ---
	select {
	case d := <-applied:
		if _, ok := d["hooks"]; !ok {
			t.Fatalf("expected 'hooks' key in OnConfigChanged desired map; got keys: %v", mapKeys(d))
		}
		t.Logf("OnConfigChanged received hooks key OK")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnConfigChanged callback")
	}

	// --- Assert shadow reconciliation: desired_ver == reported_ver in DB ---
	deadline := time.Now().Add(5 * time.Second)
	for {
		thing, err := hub.Mgr().Store().RegistryStore().GetThing(ctx, "agent-e2e")
		if err != nil {
			t.Fatalf("get thing: %v", err)
		}
		t.Logf("reconciliation check: desired_ver=%d reported_ver=%d", thing.DesiredVer, thing.ReportedVer)
		if thing.ReportedVer >= thing.DesiredVer && thing.DesiredVer > 0 {
			t.Logf("shadow reconciled: desired_ver=%d reported_ver=%d", thing.DesiredVer, thing.ReportedVer)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"shadow not reconciled after 5s: reported_ver(%d) != desired_ver(%d)",
				thing.ReportedVer, thing.DesiredVer,
			)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForConnect polls until the client reaches ModeWSConnected or the
// test deadline expires.
func waitForConnect(t *testing.T, c *thingclient.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Mode() == thingclient.ModeWSConnected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("client did not reach WSConnected within 5s (current mode: %s)", c.Mode())
}

// mapKeys returns the keys of a map for diagnostic output.
func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
