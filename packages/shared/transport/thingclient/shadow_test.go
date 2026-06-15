package thingclient

import (
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestClient creates a minimal Client for unit testing with an isolated
// Prometheus registry. It does NOT call Start() — tests drive applyConfig
// and helpers directly.
func newTestClient(t *testing.T) (*Client, prometheus.Gatherer) {
	t.Helper()
	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:            "wss://hub.test/ws",
		ThingType:         "test-thing",
		ThingID:           "thing-001",
		Token:             "test-token",
		Logger:            slog.Default(),
		MetricsRegisterer: reg,
		MetricsNamespace:  "test",
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return c, reg
}

// setWSConnected marks the client as WebSocket-connected so applyConfig can
// enqueue shadow_report messages during unit tests.
func setWSConnected(t *testing.T, c *Client) {
	t.Helper()
	c.mu.Lock()
	c.mode = ModeWSConnected
	c.mu.Unlock()
}

// drainOutCh consumes one message from the client's outbound WebSocket queue.
func drainOutCh(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.outChControl:
	case <-time.After(time.Second):
		t.Fatal("timed out draining outCh")
	}
}

func sampleDesired() map[string]ConfigState {
	return map[string]ConfigState{
		"routing": {State: json.RawMessage(`{"provider":"openai"}`), Version: 1},
	}
}

// 1. TestApplyConfig_CallsCallback

func TestApplyConfig_CallsCallback(t *testing.T) {
	c, reg := newTestClient(t)

	var got map[string]ConfigState
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		got = desired
		return desired, nil
	})

	desired := sampleDesired()
	c.applyConfig(desired, 1)

	if got == nil {
		t.Fatal("expected callback to be invoked, got nil")
	}
	if _, ok := got["routing"]; !ok {
		t.Fatal("expected 'routing' key in desired map passed to callback")
	}

	successCount := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("success"))
	if successCount != 1 {
		t.Errorf("expected config_applies{status=success} = 1, got %v", successCount)
	}

	_ = reg
}

// 2. TestApplyConfig_VersionGuard

func TestApplyConfig_VersionGuard(t *testing.T) {
	c, _ := newTestClient(t)

	called := false
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		called = true
		return desired, nil
	})

	c.reportedVer.Store(5)

	c.applyConfig(sampleDesired(), 5) // equal
	if called {
		t.Error("callback should NOT be invoked when desiredVer == reportedVer")
	}

	c.applyConfig(sampleDesired(), 3) // less
	if called {
		t.Error("callback should NOT be invoked when desiredVer < reportedVer")
	}
}

// TestApplyConfigForce_BypassesVersionGate verifies that the admin-triggered
// forced replay path invokes the OnConfigChanged callback even when
// desiredVer == reportedVer, and that it still stores reportedVer at
// desiredVer after a successful shadow_report so subsequent non-forced pushes behave normally.
//
// This is the runtime half of the "Re-sync this key" button: Hub broadcasts
// a config_changed message with Force=true at the same DesiredVer, and the
// client must re-run the reducer + emit a shadow_report instead of silently
// skipping as it would for a normal same-version message.
func TestApplyConfigForce_BypassesVersionGate(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	called := 0
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		called++
		return desired, nil
	})

	c.reportedVer.Store(5)
	c.applyConfigForce(sampleDesired(), 5)
	drainOutCh(t, c)

	if called != 1 {
		t.Errorf("callback should fire on force replay at equal version; called=%d", called)
	}
	if c.reportedVer.Load() != 5 {
		t.Errorf("reportedVer should stay at 5 after forced replay; got %d", c.reportedVer.Load())
	}

	successCount := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("success"))
	if successCount != 1 {
		t.Errorf("expected config_applies{status=success}=1; got %v", successCount)
	}
}

// TestApplyConfigForce_CallbackErrorLeavesReportedVer verifies that a failed
// forced replay does not claim success: reportedVer stays put, the failure
// metric increments, and no shadow_report is emitted (sendShadowReport is
// only called on the success branch in applyConfigForce).
func TestApplyConfigForce_CallbackErrorLeavesReportedVer(t *testing.T) {
	c, _ := newTestClient(t)

	c.OnConfigChanged(func(_ map[string]ConfigState) (map[string]ConfigState, error) {
		return nil, errors.New("apply failed")
	})

	c.reportedVer.Store(7)
	c.applyConfigForce(sampleDesired(), 7)

	if c.reportedVer.Load() != 7 {
		t.Errorf("reportedVer should remain 7 on forced-replay callback error; got %d", c.reportedVer.Load())
	}
	failCount := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("failure"))
	if failCount != 1 {
		t.Errorf("expected config_applies{status=failure}=1; got %v", failCount)
	}
}

// 3. TestApplyConfig_DuplicateVersion

func TestApplyConfig_DuplicateVersion(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	callCount := 0
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		callCount++
		return desired, nil
	})

	c.applyConfig(sampleDesired(), 3)
	drainOutCh(t, c)
	c.applyConfig(sampleDesired(), 3) // duplicate

	if callCount != 1 {
		t.Errorf("expected callback called once, got %d", callCount)
	}
}

// 4. TestApplyConfig_CallbackError

func TestApplyConfig_CallbackError(t *testing.T) {
	c, _ := newTestClient(t)

	c.OnConfigChanged(func(_ map[string]ConfigState) (map[string]ConfigState, error) {
		return nil, errors.New("apply failed")
	})

	c.applyConfig(sampleDesired(), 1)

	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer should remain 0 on callback error, got %d", c.reportedVer.Load())
	}

	failCount := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("failure"))
	if failCount != 1 {
		t.Errorf("expected config_applies{status=failure} = 1, got %v", failCount)
	}

	successCount := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("success"))
	if successCount != 0 {
		t.Errorf("expected config_applies{status=success} = 0, got %v", successCount)
	}
}

// 5. TestApplyConfig_NoCallback

func TestApplyConfig_NoCallback(t *testing.T) {
	c, _ := newTestClient(t)

	// onConfigChanged is nil by default — should log warning, not panic.
	c.applyConfig(sampleDesired(), 1)

	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer should remain 0 when no callback, got %d", c.reportedVer.Load())
	}
}

// 6. TestShadowReport_WSMode

func TestShadowReport_WSMode(t *testing.T) {
	c, _ := newTestClient(t)

	c.mu.Lock()
	c.mode = ModeWSConnected
	c.mu.Unlock()

	reported := map[string]ConfigState{
		"routing": {State: json.RawMessage(`{"applied":true}`), Version: 1},
	}
	if err := c.sendShadowReport(reported, 2); err != nil {
		t.Fatalf("sendShadowReport: %v", err)
	}

	select {
	case raw := <-c.outChControl:
		var msg thingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal outCh message: %v", err)
		}
		if msg.Type != "shadow_report" {
			t.Errorf("expected type 'shadow_report', got %q", msg.Type)
		}
		if msg.ReportedVer != 2 {
			t.Errorf("expected reportedVer=2, got %d", msg.ReportedVer)
		}
		if _, ok := msg.Reported["routing"]; !ok {
			t.Error("expected 'routing' key in reported map")
		}
	case <-time.After(time.Second):
		t.Fatal("expected message on outCh, timed out")
	}

	successCount := testutil.ToFloat64(c.promMetrics.shadowReports.WithLabelValues("success"))
	if successCount != 1 {
		t.Errorf("expected shadow_reports{status=success} = 1, got %v", successCount)
	}
}

// TestShadowReport_WireFormat_IsFlatRawState locks in the contract that
// shadow_report carries per-key raw state, NOT the old {state, version}
// wrapper. The wrapper caused thing.reported to diverge from thing.desired
// in Hub storage (desired is stored raw via jsonb_set) and produced
// spurious "out of sync" diffs in the admin UI even when content matched.
// Per-key version metadata travels on ReportedVer / KeyVersions instead.
func TestShadowReport_WireFormat_IsFlatRawState(t *testing.T) {
	c, _ := newTestClient(t)

	c.mu.Lock()
	c.mode = ModeWSConnected
	c.mu.Unlock()

	rawState := `{"log_level":"info","metrics_enabled":true,"prometheus_path":"/metrics","tracing_enabled":false}`
	reported := map[string]ConfigState{
		"observability": {State: json.RawMessage(rawState), Version: 3},
	}
	if err := c.sendShadowReport(reported, 3); err != nil {
		t.Fatalf("sendShadowReport: %v", err)
	}

	select {
	case raw := <-c.outChControl:
		// Decode into a loose shape so we can assert the exact JSON the
		// value side has — raw state object, not the legacy wrapper.
		var envelope struct {
			Reported map[string]json.RawMessage `json:"reported"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("unmarshal outCh message: %v", err)
		}
		got, ok := envelope.Reported["observability"]
		if !ok {
			t.Fatalf("observability missing from reported map: %s", raw)
		}
		// Byte-level comparison — the field must be the raw state object,
		// not re-wrapped with state/version keys.
		if string(got) != rawState {
			t.Errorf("reported[observability] = %s; want flat raw state %s", got, rawState)
		}
		// Negative check: the wrapper keys must not appear in the value.
		var asMap map[string]any
		if err := json.Unmarshal(got, &asMap); err == nil {
			if _, bad := asMap["state"]; bad {
				t.Errorf("reported[observability] still contains 'state' wrapper key: %s", got)
			}
			if _, bad := asMap["version"]; bad {
				t.Errorf("reported[observability] still contains 'version' wrapper key: %s", got)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("expected message on outCh, timed out")
	}
}

// 7. TestShadowReport_Disconnected

func TestShadowReport_Disconnected(t *testing.T) {
	c, _ := newTestClient(t)

	// mode defaults to ModeDisconnected
	reported := map[string]ConfigState{
		"routing": {State: json.RawMessage(`{}`), Version: 1},
	}
	if err := c.sendShadowReport(reported, 1); err == nil {
		t.Fatal("expected error when disconnected")
	}

	select {
	case <-c.outChControl:
		t.Fatal("no message should be sent when disconnected")
	default:
	}

	failCount := testutil.ToFloat64(c.promMetrics.shadowReports.WithLabelValues("failure"))
	if failCount != 1 {
		t.Errorf("expected shadow_reports{status=failure} = 1, got %v", failCount)
	}
}

// 8. TestVersionTracking_InitialConnect

func TestVersionTracking_InitialConnect(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	if c.reportedVer.Load() != 0 {
		t.Fatalf("reportedVer should start at 0, got %d", c.reportedVer.Load())
	}

	called := false
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		called = true
		return desired, nil
	})

	c.applyConfig(sampleDesired(), 1)
	drainOutCh(t, c)

	if !called {
		t.Error("callback should fire on first apply with ver > 0")
	}
	if c.reportedVer.Load() != 1 {
		t.Errorf("reportedVer should be 1 after apply, got %d", c.reportedVer.Load())
	}
}

// 9. TestVersionTracking_Reconnect_InSync

func TestVersionTracking_Reconnect_InSync(t *testing.T) {
	c, _ := newTestClient(t)

	c.reportedVer.Store(5)

	called := false
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		called = true
		return desired, nil
	})

	c.applyConfig(sampleDesired(), 5) // same version

	if called {
		t.Error("callback should NOT fire when desiredVer == reportedVer on reconnect")
	}
}

// 10. TestVersionTracking_Reconnect_Changed

func TestVersionTracking_Reconnect_Changed(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	c.reportedVer.Store(3)

	called := false
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		called = true
		return desired, nil
	})

	c.applyConfig(sampleDesired(), 5) // higher version
	drainOutCh(t, c)

	if !called {
		t.Error("callback should fire when desiredVer > reportedVer on reconnect")
	}
	if c.reportedVer.Load() != 5 {
		t.Errorf("reportedVer should be updated to 5, got %d", c.reportedVer.Load())
	}
}

// 11. TestInSync_True

func TestInSync_True(t *testing.T) {
	c, _ := newTestClient(t)

	c.desiredVer.Store(5)
	c.reportedVer.Store(5)
	if !c.InSync() {
		t.Error("InSync() should be true when reportedVer == desiredVer")
	}

	c.reportedVer.Store(6) // reported ahead
	if !c.InSync() {
		t.Error("InSync() should be true when reportedVer > desiredVer")
	}
}

// 12. TestInSync_False

func TestInSync_False(t *testing.T) {
	c, _ := newTestClient(t)

	c.desiredVer.Store(5)
	c.reportedVer.Store(3)
	if c.InSync() {
		t.Error("InSync() should be false when reportedVer < desiredVer")
	}
}

// 13. TestDesiredVer_ReportedVer

func TestDesiredVer_ReportedVer(t *testing.T) {
	c, _ := newTestClient(t)

	if c.DesiredVer() != 0 {
		t.Errorf("DesiredVer() should start at 0, got %d", c.DesiredVer())
	}
	if c.ReportedVer() != 0 {
		t.Errorf("ReportedVer() should start at 0, got %d", c.ReportedVer())
	}

	c.desiredVer.Store(10)
	c.reportedVer.Store(7)

	if c.DesiredVer() != 10 {
		t.Errorf("DesiredVer() expected 10, got %d", c.DesiredVer())
	}
	if c.ReportedVer() != 7 {
		t.Errorf("ReportedVer() expected 7, got %d", c.ReportedVer())
	}
}

// LastReportedAtTime + HeartbeatInterval

func TestLastReportedAtTime_ZeroBeforeFirstReport(t *testing.T) {
	c, _ := newTestClient(t)
	if got := c.LastReportedAtTime(); !got.IsZero() {
		t.Errorf("LastReportedAtTime() before first report = %v, want zero", got)
	}
}

func TestLastReportedAtTime_ReadsAtomic(t *testing.T) {
	c, _ := newTestClient(t)

	before := time.Now().UTC()
	c.lastReportedAt.Store(before.Format(time.RFC3339))
	c.lastReportedAtNanos.Store(before.UnixNano())

	got := c.LastReportedAtTime()
	if !got.Equal(before) {
		t.Errorf("LastReportedAtTime() = %v, want %v", got, before)
	}
}

func TestHeartbeatInterval_ReturnsConfig(t *testing.T) {
	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:            "wss://hub.test/ws",
		ThingType:         "test-thing",
		ThingID:           "thing-001",
		Token:             "test-token",
		Logger:            slog.Default(),
		MetricsRegisterer: reg,
		MetricsNamespace:  "test",
		HeartbeatInterval: 7 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.HeartbeatInterval(); got != 7*time.Second {
		t.Errorf("HeartbeatInterval() = %v, want 7s", got)
	}
}

// --- F-0121: partial-apply shadow report on the error path ---

// partialApplyCallback returns a callback that mimics configloader.Loader.Apply:
// continue-on-error. It applies every key in `succeed` (recording a success
// outcome and echoing the desired state into reported) and fails every key in
// `fail` (recording an error outcome), then returns the partial reported map
// plus a non-nil first error. desiredVer is the per-key version stamped on each
// outcome, matching how the Loader records cs.Version.
func partialApplyCallback(c *Client, succeed []string, fail map[string]string, desiredVer int64) OnConfigChangedFunc {
	return func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		reported := make(map[string]ConfigState, len(succeed))
		for _, k := range succeed {
			cs := desired[k]
			c.Outcomes().Record(k, desiredVer, nil)
			reported[k] = ConfigState{State: cs.State, Version: cs.Version}
		}
		var firstErr error
		for k, msg := range fail {
			err := errors.New(msg)
			c.Outcomes().Record(k, desiredVer, err)
			if firstErr == nil {
				firstErr = err
			}
		}
		return reported, firstErr
	}
}

func threeKeyDesired() map[string]ConfigState {
	return map[string]ConfigState{
		"routing":       {State: json.RawMessage(`{"provider":"openai"}`), Version: 7},
		"observability": {State: json.RawMessage(`{"log_level":"info"}`), Version: 7},
		"killswitch":    {State: json.RawMessage(`{"enabled":false}`), Version: 7},
	}
}

// readShadowReport drains one shadow_report off the WS control queue and
// decodes it. Fails the test if nothing arrives.
func readShadowReport(t *testing.T, c *Client) thingMessage {
	t.Helper()
	select {
	case raw := <-c.outChControl:
		var msg thingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal shadow_report: %v", err)
		}
		return msg
	case <-time.After(time.Second):
		t.Fatal("expected a shadow_report on the error path, got none")
		return thingMessage{}
	}
}

// TestApplyConfig_PartialFailure_SendsPartialReport is the core F-0121
// regression: when 1 of 3 keys fails to apply, the node must NOT go dark.
// The shadow_report is still sent carrying the 2 succeeded keys in `reported`
// plus a per-key `reportedOutcomes` ledger (2 success + 1 error with detail),
// while the global reportedVer stays behind desiredVer so the node correctly
// shows drift.
func TestApplyConfig_PartialFailure_SendsPartialReport(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	c.OnConfigChanged(partialApplyCallback(c,
		[]string{"routing", "observability"},
		map[string]string{"killswitch": "killswitch apply failed"},
		7,
	))

	c.applyConfig(threeKeyDesired(), 7)
	msg := readShadowReport(t, c)

	// Wire version stays at the OLD reported version (0) — the node has not
	// converged, so Hub must keep showing it out of sync vs desired_ver=7.
	if msg.ReportedVer != 0 {
		t.Errorf("wire reportedVer = %d, want 0 (not converged on partial failure)", msg.ReportedVer)
	}
	if c.reportedVer.Load() != 0 {
		t.Errorf("client reportedVer advanced to %d on partial failure; want 0", c.reportedVer.Load())
	}

	// reported map carries the 2 succeeded keys, NOT the failed one.
	if _, ok := msg.Reported["routing"]; !ok {
		t.Error("reported map missing succeeded key 'routing'")
	}
	if _, ok := msg.Reported["observability"]; !ok {
		t.Error("reported map missing succeeded key 'observability'")
	}
	if _, ok := msg.Reported["killswitch"]; ok {
		t.Error("reported map must NOT contain the failed key 'killswitch'")
	}

	// Per-key outcomes: 2 success (appliedVersion=7, no error), 1 error.
	for _, k := range []string{"routing", "observability"} {
		oc, ok := msg.ReportedOutcomes[k]
		if !ok {
			t.Fatalf("outcomes missing succeeded key %q", k)
		}
		if oc.ApplyError != nil {
			t.Errorf("succeeded key %q has unexpected applyError %q", k, oc.ApplyError.Message)
		}
		if oc.AppliedVersion == nil || *oc.AppliedVersion != 7 {
			t.Errorf("succeeded key %q appliedVersion = %v, want 7", k, oc.AppliedVersion)
		}
	}
	failOC, ok := msg.ReportedOutcomes["killswitch"]
	if !ok {
		t.Fatal("outcomes missing failed key 'killswitch'")
	}
	if failOC.ApplyError == nil {
		t.Fatal("failed key 'killswitch' has no applyError")
	}
	if failOC.ApplyError.Message != "killswitch apply failed" {
		t.Errorf("applyError.Message = %q, want %q", failOC.ApplyError.Message, "killswitch apply failed")
	}
	if failOC.AppliedVersion != nil {
		t.Errorf("never-applied key 'killswitch' has appliedVersion %v, want nil", *failOC.AppliedVersion)
	}

	// Metrics: counted as a failure, not a success.
	if got := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("failure")); got != 1 {
		t.Errorf("config_applies{failure} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("success")); got != 0 {
		t.Errorf("config_applies{success} = %v, want 0", got)
	}
}

// TestApplyConfig_AllFailure_StillSendsReport verifies the all-fail path is no
// longer silent: a report is sent with an empty (non-nil) reported map and an
// all-error outcomes ledger, and reportedVer does not advance.
func TestApplyConfig_AllFailure_StillSendsReport(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	c.OnConfigChanged(partialApplyCallback(c,
		nil,
		map[string]string{"routing": "routing blew up"},
		5,
	))

	c.applyConfig(sampleDesired(), 5)
	msg := readShadowReport(t, c)

	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer advanced to %d on all-fail; want 0", c.reportedVer.Load())
	}
	// No key applied, so the `reported` map carries no entries (the WS frame
	// omits the empty map via omitempty); the apply detail rides entirely in
	// reportedOutcomes below.
	if len(msg.Reported) != 0 {
		t.Errorf("reported map should be empty on all-fail, got %v", msg.Reported)
	}
	oc, ok := msg.ReportedOutcomes["routing"]
	if !ok || oc.ApplyError == nil {
		t.Fatalf("expected an error outcome for 'routing', got %+v", oc)
	}
	if oc.ApplyError.Message != "routing blew up" {
		t.Errorf("applyError.Message = %q, want %q", oc.ApplyError.Message, "routing blew up")
	}
}

// TestApplyConfig_PartialFailure_Disconnected covers the error-path send
// failure branch: when not connected, sendShadowReport returns an error which
// is logged (not fatal) and reportedVer still does not advance.
func TestApplyConfig_PartialFailure_Disconnected(t *testing.T) {
	c, _ := newTestClient(t)
	// mode stays ModeDisconnected — sendShadowReport will fail.

	c.OnConfigChanged(partialApplyCallback(c,
		[]string{"routing"},
		map[string]string{"observability": "boom"},
		3,
	))

	c.applyConfig(threeKeyDesired(), 3)

	select {
	case <-c.outChControl:
		t.Fatal("no message should reach the queue while disconnected")
	default:
	}
	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer advanced to %d; want 0", c.reportedVer.Load())
	}
	// The partial-send attempt still counts as a shadow_report failure.
	if got := testutil.ToFloat64(c.promMetrics.shadowReports.WithLabelValues("failure")); got != 1 {
		t.Errorf("shadow_reports{failure} = %v, want 1", got)
	}
}

// TestApplyConfigForce_PartialFailure_SendsPartialReport verifies the forced
// "Re-sync this key" replay also emits partial detail on failure: the report is
// sent at the current reportedVer with per-key outcomes, and reportedVer is not
// advanced past the failed apply.
func TestApplyConfigForce_PartialFailure_SendsPartialReport(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	c.reportedVer.Store(7)
	c.OnConfigChanged(partialApplyCallback(c,
		[]string{"routing"},
		map[string]string{"killswitch": "resync still failing"},
		7,
	))

	c.applyConfigForce(threeKeyDesired(), 7)
	msg := readShadowReport(t, c)

	if msg.ReportedVer != 7 {
		t.Errorf("forced wire reportedVer = %d, want 7 (current ver)", msg.ReportedVer)
	}
	if c.reportedVer.Load() != 7 {
		t.Errorf("forced reportedVer = %d, want 7 (unchanged)", c.reportedVer.Load())
	}
	if _, ok := msg.Reported["routing"]; !ok {
		t.Error("forced report missing succeeded key 'routing'")
	}
	if oc, ok := msg.ReportedOutcomes["killswitch"]; !ok || oc.ApplyError == nil {
		t.Fatalf("forced report missing error outcome for 'killswitch': %+v", oc)
	}
	if got := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("failure")); got != 1 {
		t.Errorf("config_applies{failure} = %v, want 1", got)
	}
}

// TestApplyConfigForce_PartialFailure_Disconnected covers the forced error-path
// send-failure branch (sendShadowReport returns an error, logged not fatal).
func TestApplyConfigForce_PartialFailure_Disconnected(t *testing.T) {
	c, _ := newTestClient(t)
	c.reportedVer.Store(4)

	c.OnConfigChanged(partialApplyCallback(c,
		[]string{"routing"},
		map[string]string{"killswitch": "boom"},
		4,
	))

	c.applyConfigForce(threeKeyDesired(), 4)

	select {
	case <-c.outChControl:
		t.Fatal("no message should reach the queue while disconnected")
	default:
	}
	if c.reportedVer.Load() != 4 {
		t.Errorf("reportedVer = %d, want 4 (unchanged)", c.reportedVer.Load())
	}
	if got := testutil.ToFloat64(c.promMetrics.shadowReports.WithLabelValues("failure")); got != 1 {
		t.Errorf("shadow_reports{failure} = %v, want 1", got)
	}
}
