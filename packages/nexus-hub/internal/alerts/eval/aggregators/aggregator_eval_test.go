package aggregators

import (
	"encoding/json"
	"testing"
	"time"

	alerteval "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// Test helpers

func strPtr(s string) *string     { return &s }
func intPtr(i int) *int           { return &i }
func floatPtr(f float64) *float64 { return &f }

func trafficEvent(t time.Time, traffic *consumer.TrafficEventMessage) *alerteval.Event {
	return &alerteval.Event{
		Kind:      alerteval.EventTraffic,
		Source:    alerteval.SourceAITraffic,
		Timestamp: t,
		Traffic:   traffic,
	}
}

func auditEvent(t time.Time, audit *mq.AdminAuditMessage) *alerteval.Event {
	return &alerteval.Event{
		Kind:      alerteval.EventAudit,
		Source:    alerteval.SourceAdminAudit,
		Timestamp: t,
		Audit:     audit,
	}
}

// fireFromTick is a convenience wrapper that returns the first Decision
// from Tick, or nil if none. Most aggregator tests examine only one fire.
func fireFromTick(decisions []alerteval.Decision) *alerteval.Decision {
	if len(decisions) == 0 {
		return nil
	}
	return &decisions[0]
}

// helpers.go — additional gap coverage (EvalCountInWindow Resolve,
// EvalRatioInWindow recovery, EvalSumInWindow recovery,
// EvalCompareToBaseline no-fire paths, EvalPercentileBaseline)

func TestEvalCountInWindow_ResolveAfterFire(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()

	// Pump 25 events at now-1h so the lookback window has dropped them.
	w := rt.Window("k", 300)
	for range 25 {
		w.Add(now.Add(-time.Hour), 1, 0)
	}
	// Stamp a cooldown so HasFired returns true.
	rt.SetCooldown("k", now.Add(time.Hour))

	d := EvalCountInWindow(rt, "k", 300, 20, now, "msg")
	if d == nil || d.Action != alerteval.Resolve {
		t.Fatalf("expected Resolve after fire+drop, got %+v", d)
	}
}

func TestEvalRatioInWindow_ResolveAfterFire_BelowSamples(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()

	w := rt.Window("k", 300)
	// Old events outside the window.
	for range 30 {
		w.Add(now.Add(-time.Hour), 1, 1)
	}
	rt.SetCooldown("k", now.Add(time.Hour))

	d := EvalRatioInWindow(rt, "k", 300, 5, 20, now, "msg")
	if d == nil || d.Action != alerteval.Resolve {
		t.Fatalf("expected Resolve (samples below min) after fire, got %+v", d)
	}
}

func TestEvalRatioInWindow_ResolveAfterFire_BelowHysteresis(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("k", 300)
	// Add 50 samples with a low ratio (1% fail) — below thresholdPct(5)-1pp.
	for range 49 {
		w.Add(now, 0, 1)
	}
	w.Add(now, 1, 1) // 1/50 = 2%
	rt.SetCooldown("k", now.Add(time.Hour))

	d := EvalRatioInWindow(rt, "k", 300, 5, 20, now, "msg")
	if d == nil || d.Action != alerteval.Resolve {
		t.Fatalf("expected Resolve (below hysteresis) after fire, got %+v", d)
	}
}

func TestEvalRatioInWindow_NoFireBetweenThresholdAndHysteresis(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("k", 300)
	// Ratio sits between (threshold-1)pp and threshold — no resolve, no fire.
	// threshold=5, so 4.5% sits in dead-zone.
	for range 200 {
		w.Add(now, 0, 1)
	}
	for range 9 {
		w.Add(now, 1, 1) // 9 / 209 ≈ 4.3%
	}
	rt.SetCooldown("k", now.Add(time.Hour))

	d := EvalRatioInWindow(rt, "k", 300, 5, 20, now, "msg")
	if d != nil {
		t.Errorf("expected nil decision in dead-zone, got %+v", d)
	}
}

func TestEvalSumInWindow_ResolveAfterFire(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("k", 300)
	w.Add(now.Add(-time.Hour), 100.0, 1) // out of window
	rt.SetCooldown("k", now.Add(time.Hour))

	d := EvalSumInWindow(rt, "k", 300, 50.0, now, "cost")
	if d == nil || d.Action != alerteval.Resolve {
		t.Fatalf("expected Resolve, got %+v", d)
	}
}

func TestEvalSumInWindow_NoDecisionBelowThresholdNoFire(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("k", 300)
	w.Add(now, 5.0, 1)

	d := EvalSumInWindow(rt, "k", 300, 50.0, now, "cost")
	if d != nil {
		t.Errorf("expected nil, got %+v", d)
	}
}

func TestEvalCompareToBaseline_NoFireBelowAbsFloor(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("k", 3900)
	w.Add(now, 1, 0)

	d := EvalCompareToBaseline(rt, "k", 300, 3600, 10, 50, now, "spike")
	if d != nil {
		t.Errorf("expected nil (below absFloorReq), got %+v", d)
	}
}

func TestEvalCompareToBaseline_NoFireSimilarToBaseline(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.Window("k", 3900)
	// Steady traffic, 50 per 5min consistently.
	for chunk := range 13 {
		for i := range 50 {
			w.Add(now.Add(-time.Duration(chunk)*5*time.Minute).Add(-time.Duration(i)*time.Second), 1, 0)
		}
	}
	d := EvalCompareToBaseline(rt, "k", 300, 3600, 10, 50, now, "spike")
	if d != nil {
		t.Errorf("expected nil (no spike), got %+v", d)
	}
}

func TestEvalCompareToBaseline_ChunksClampedToOne(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	// alertWindowSec > baselineWindowSec — chunks would be <1, clamp to 1.
	w := rt.Window("k", 800)
	// last 500s: many events
	for i := range 200 {
		w.Add(now.Add(-time.Duration(i)*time.Second), 1, 0)
	}
	// no events in baseline (0..300 of older zone)
	d := EvalCompareToBaseline(rt, "k", 500, 300, 2.0, 50, now, "spike")
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire with clamped chunks=1, got %+v", d)
	}
}

func TestEvalPercentileBaseline_BelowMinSamples(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.SampleWindow("k", 1000)
	for range 5 {
		w.Add(now, 5000)
	}
	d := EvalPercentileBaseline(rt, "k", 300, 3600, 95, 2.0, 1000, 10, 1000, now, "msg")
	if d != nil {
		t.Errorf("expected nil (below minSamples), got %+v", d)
	}
}

func TestEvalPercentileBaseline_BelowAbsFloor(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.SampleWindow("k", 1000)
	for range 100 {
		w.Add(now, 50) // way below 1000ms absFloor
	}
	d := EvalPercentileBaseline(rt, "k", 300, 3600, 95, 2.0, 1000, 10, 1000, now, "msg")
	if d != nil {
		t.Errorf("expected nil (below absFloor), got %+v", d)
	}
}

func TestEvalPercentileBaseline_FiresWhenAlertExceedsBaseline(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.SampleWindow("k", 5000)
	// Baseline samples — 30min ago — low latency 500ms. We need many
	// (>>20 * alert-samples) so that p95 over (baseline+alert) is
	// dominated by the baseline tier and stays well below the alert
	// tier value. Nearest-rank p95 with 4020 samples: idx = 0.95 *
	// 4019 = 3818, which lands in the baseline 500ms tier (samples
	// 0..3999 sorted). Alert window of 20 samples at 8000ms → 8000 >>
	// 1.5*500. Fire.
	for range 4000 {
		w.Add(now.Add(-30*time.Minute), 500)
	}
	for i := range 20 {
		w.Add(now.Add(-time.Duration(i)*time.Second), 8000)
	}
	d := EvalPercentileBaseline(rt, "k", 300, 3600, 95, 1.5, 1000, 10, 5000, now, "lat")
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}

func TestEvalPercentileBaseline_NoFireWhenBaselineIsZero(t *testing.T) {
	rt := alerteval.NewRuntime("test", time.Now().Add(-time.Hour))
	now := time.Now()
	w := rt.SampleWindow("k", 1000)
	// All samples within alert window only; baseline-window query returns
	// the same samples → basePct equals alertPct → not strictly greater.
	for i := range 50 {
		w.Add(now.Add(-time.Duration(i)*time.Second), 5000)
	}
	d := EvalPercentileBaseline(rt, "k", 300, 3600, 95, 2.0, 1000, 10, 1000, now, "msg")
	if d != nil {
		t.Errorf("expected nil, got %+v", d)
	}
}

// util.go — param helpers

func TestIntParam_TypeVariations(t *testing.T) {
	if got := intParam(nil, "x", 7); got != 7 {
		t.Errorf("nil → default; got %d", got)
	}
	if got := intParam(map[string]any{"x": 3}, "x", 7); got != 3 {
		t.Errorf("int branch; got %d", got)
	}
	if got := intParam(map[string]any{"x": int64(4)}, "x", 7); got != 4 {
		t.Errorf("int64 branch; got %d", got)
	}
	if got := intParam(map[string]any{"x": 5.5}, "x", 7); got != 5 {
		t.Errorf("float64 branch; got %d", got)
	}
	if got := intParam(map[string]any{"x": "bad"}, "x", 7); got != 7 {
		t.Errorf("bad type → default; got %d", got)
	}
	if got := intParam(map[string]any{}, "missing", 7); got != 7 {
		t.Errorf("missing key → default; got %d", got)
	}
}

func TestFloatParam_TypeVariations(t *testing.T) {
	if got := floatParam(nil, "x", 1.5); got != 1.5 {
		t.Errorf("nil → default; got %f", got)
	}
	if got := floatParam(map[string]any{"x": 3.5}, "x", 1.5); got != 3.5 {
		t.Errorf("float64; got %f", got)
	}
	if got := floatParam(map[string]any{"x": 4}, "x", 1.5); got != 4.0 {
		t.Errorf("int branch; got %f", got)
	}
	if got := floatParam(map[string]any{"x": int64(5)}, "x", 1.5); got != 5.0 {
		t.Errorf("int64 branch; got %f", got)
	}
	if got := floatParam(map[string]any{"x": "bad"}, "x", 1.5); got != 1.5 {
		t.Errorf("bad type; got %f", got)
	}
}

func TestStringParam_TypeVariations(t *testing.T) {
	if got := stringParam(nil, "x", "def"); got != "def" {
		t.Errorf("nil → default; got %q", got)
	}
	if got := stringParam(map[string]any{"x": "yes"}, "x", "def"); got != "yes" {
		t.Errorf("string; got %q", got)
	}
	if got := stringParam(map[string]any{"x": 5}, "x", "def"); got != "def" {
		t.Errorf("non-string → default; got %q", got)
	}
}

func TestDerefHelpers(t *testing.T) {
	if derefString(nil) != "" {
		t.Error("derefString nil → \"\"")
	}
	if derefString(strPtr("v")) != "v" {
		t.Error("derefString value")
	}
	if derefInt(nil) != 0 {
		t.Error("derefInt nil → 0")
	}
	if derefInt(intPtr(7)) != 7 {
		t.Error("derefInt value")
	}
	if derefFloat(nil) != 0 {
		t.Error("derefFloat nil → 0")
	}
	if derefFloat(floatPtr(1.5)) != 1.5 {
		t.Error("derefFloat value")
	}
}

func TestStringInSlice(t *testing.T) {
	if !stringInSlice("a", []string{"a", "b"}) {
		t.Error("expected true")
	}
	if stringInSlice("z", []string{"a", "b"}) {
		t.Error("expected false")
	}
	if stringInSlice("x", nil) {
		t.Error("nil slice → false")
	}
}

func TestAuthInvalidKeyBurst_Metadata(t *testing.T) {
	a := NewAuthInvalidKeyBurst()
	if a.RuleID() != "auth.invalid_key_burst" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 || a.Sources()[0] != alerteval.SourceAITraffic {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(map[string]any{"windowSec": 600}) != 600 {
		t.Error("MinWarmupSec param honored")
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestAuthInvalidKeyBurst_OnEventFiltering(t *testing.T) {
	a := NewAuthInvalidKeyBurst()
	rt := alerteval.NewRuntime("auth.invalid_key_burst", time.Now().Add(-time.Hour))
	now := time.Now()

	// Wrong kind → ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	// nil traffic → ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Wrong error code → ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		ErrorCode: strPtr("OTHER"),
		SourceIP:  strPtr("1.2.3.4"),
	}))
	// Missing IP → ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		ErrorCode: strPtr("AUTH_INVALID"),
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets after filtering, got %v", rt.Targets())
	}
}

func TestAuthInvalidKeyBurst_FiresOnBurst(t *testing.T) {
	a := NewAuthInvalidKeyBurst()
	rt := alerteval.NewRuntime("auth.invalid_key_burst", time.Now().Add(-time.Hour))
	now := time.Now()
	for range 25 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			ErrorCode: strPtr("AUTH_INVALID"),
			SourceIP:  strPtr("1.2.3.4"),
		}))
	}
	// Also accepts AUTH_KEY_EXPIRED.
	for range 5 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			ErrorCode: strPtr("AUTH_KEY_EXPIRED"),
			SourceIP:  strPtr("1.2.3.4"),
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 20}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "ip:1.2.3.4" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestComplianceHookExecutionTimeoutSurge_Metadata(t *testing.T) {
	a := NewComplianceHookExecutionTimeoutSurge()
	if a.RuleID() != "compliance.hook_execution_timeout_surge" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 3 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestWalkHookTimeoutsByID(t *testing.T) {
	// Nil + empty + malformed are all silently skipped.
	out := walkHookTimeoutsByID(nil, []byte(""))
	if len(out) != 0 {
		t.Errorf("nil+empty should be empty: %v", out)
	}

	out = walkHookTimeoutsByID([]byte("not-json"), json.RawMessage("[bad"))
	if len(out) != 0 {
		t.Errorf("malformed should be skipped: %v", out)
	}

	// Default-typed value (non []byte / json.RawMessage / nil) goes to default branch.
	out = walkHookTimeoutsByID(42, "string")
	if len(out) != 0 {
		t.Errorf("non-byte input should be skipped: %v", out)
	}

	// Valid request + response, with timeout + non-timeout + missing HookID + missing Error.
	req := []byte(`[
		{"hookId":"a","error":"deadline exceeded"},
		{"hookId":"a","error":"context deadline exceeded"},
		{"hookId":"b","error":"upstream Timeout occurred"},
		{"hookId":"c","error":"some other failure"},
		{"hookId":"","error":"timeout"},
		{"hookId":"d","error":""}
	]`)
	resp := json.RawMessage(`[{"hookId":"a","error":"TIMEOUT"}]`)

	out = walkHookTimeoutsByID(req, resp)
	if out["a"] != 3 {
		t.Errorf("hook a: %d", out["a"])
	}
	if out["b"] != 1 {
		t.Errorf("hook b: %d", out["b"])
	}
	if out["c"] != 0 {
		t.Errorf("hook c (non-timeout error) shouldn't count: %d", out["c"])
	}
	if out["d"] != 0 {
		t.Errorf("hook d (empty error) shouldn't count: %d", out["d"])
	}
}

func TestComplianceHookExecutionTimeoutSurge_OnEventAndTick(t *testing.T) {
	a := NewComplianceHookExecutionTimeoutSurge()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Non-traffic event ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	// nil traffic ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})

	// 25 events each carrying 1 timeout for hook-x.
	pipe := json.RawMessage(`[{"hookId":"hook-x","error":"deadline exceeded"}]`)
	for range 25 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			RequestHooksPipeline: pipe,
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 20}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "hook:hook-x" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestCompliancePayloadCaptureFailureRate_Metadata(t *testing.T) {
	a := NewCompliancePayloadCaptureFailureRate()
	if a.RuleID() != "compliance.payload_capture_failure_rate" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 3 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 600 {
		t.Error("MinWarmupSec default")
	}
}

func TestCompliancePayloadCaptureFailureRate_FilteringAndFire(t *testing.T) {
	a := NewCompliancePayloadCaptureFailureRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Non-traffic ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	// nil traffic ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Empty thingKey (no SourceProcess, no Source) ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		RequestBody: audit.Body{Kind: audit.BodyInline, Truncated: true},
	}))
	// Both directions absent ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		SourceProcess: strPtr("p"),
		RequestBody:   audit.Body{Kind: audit.BodyAbsent},
		ResponseBody:  audit.Body{Kind: audit.BodyAbsent},
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets after filtering, got %v", rt.Targets())
	}

	// SourceProcess empty → falls back to t.Source.
	for range 10 {
		// non-truncated
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			Source:       "ai-gateway",
			RequestBody:  audit.Body{Kind: audit.BodyInline},
			ResponseBody: audit.Body{Kind: audit.BodyInline},
		}))
	}
	for range 15 {
		// truncated on response
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			Source:       "ai-gateway",
			ResponseBody: audit.Body{Kind: audit.BodyInline, Truncated: true},
		}))
	}
	// 15 truncated / 25 total = 60% > 10% threshold.
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 600, "thresholdPct": 10, "minSamples": 20}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}

func TestCredentialAuthFailuresCascade_Metadata(t *testing.T) {
	a := NewCredentialAuthFailuresCascade()
	if a.RuleID() != "credential.auth_failures_cascade" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 600 {
		t.Error("MinWarmupSec default")
	}
}

func TestExtractCredentialID(t *testing.T) {
	if extractCredentialID(nil) != "" {
		t.Error("nil traffic → empty")
	}
	if extractCredentialID(&consumer.TrafficEventMessage{}) != "" {
		t.Error("nil ptr field → empty")
	}
	if got := extractCredentialID(&consumer.TrafficEventMessage{CredentialID: strPtr("cred-1")}); got != "cred-1" {
		t.Errorf("got %s", got)
	}
}

func TestCredentialAuthFailuresCascade_OnEventAndFire(t *testing.T) {
	a := NewCredentialAuthFailuresCascade()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Non-traffic ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Missing credID ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		StatusCode: intPtr(401),
	}))

	// 10 OK responses, 5 401s with no errorCode (= upstream auth fail).
	for range 10 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			CredentialID: strPtr("c1"),
			StatusCode:   intPtr(200),
		}))
	}
	for range 5 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			CredentialID: strPtr("c1"),
			StatusCode:   intPtr(401),
		}))
	}
	// 5/15 = 33% > 20%
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 600, "thresholdPct": 20, "minSamples": 10}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}

	// Also: 403 with errorCode (= Nexus-side) does NOT count as auth fail
	rt2 := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	for range 30 {
		a.OnEvent(rt2, trafficEvent(now, &consumer.TrafficEventMessage{
			CredentialID: strPtr("c2"),
			StatusCode:   intPtr(403),
			ErrorCode:    strPtr("HOOK_REJECT"),
		}))
	}
	d2 := fireFromTick(a.Tick(rt2, map[string]any{"windowSec": 600, "thresholdPct": 20, "minSamples": 10}, now))
	if d2 != nil {
		t.Errorf("expected nil (all errorCode-classified), got %+v", d2)
	}
}

func TestHookRejectRate_Metadata(t *testing.T) {
	a := NewHookRejectRate()
	if a.RuleID() != "hook.reject_rate" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 3 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestHookRejectRate_OnEventAndFire(t *testing.T) {
	a := NewHookRejectRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Non-traffic / nil / empty-thing ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{}))

	for range 50 {
		// non-reject (decision OK)
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess:       strPtr("ai-gateway-a"),
			RequestHookDecision: strPtr("ALLOW"),
		}))
	}
	for range 10 {
		// reject (request)
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess:       strPtr("ai-gateway-a"),
			RequestHookDecision: strPtr("REJECT_HARD"),
		}))
	}
	for range 5 {
		// reject (response BLOCK_SOFT)
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess:        strPtr("ai-gateway-a"),
			ResponseHookDecision: strPtr("BLOCK_SOFT"),
		}))
	}
	// 15/65 ≈ 23% > 5%
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdPct": 5, "minSamples": 20}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "thing:ai-gateway-a" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestHookRejectRate_FallbackThingKeyFromSource(t *testing.T) {
	a := NewHookRejectRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		Source:              "ai-gateway", // fallback because SourceProcess nil
		RequestHookDecision: strPtr("ALLOW"),
	}))
	targets := rt.Targets()
	if len(targets) != 1 || targets[0] != "thing:ai-gateway" {
		t.Errorf("expected fallback target thing:ai-gateway, got %v", targets)
	}
}

func TestLoginFailureFlood_Metadata(t *testing.T) {
	a := NewLoginFailureFlood()
	if a.RuleID() != "auth.login_failure_rate" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 || a.Sources()[0] != alerteval.SourceAdminAudit {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestLoginFailureFlood_OnEventFiltering(t *testing.T) {
	a := NewLoginFailureFlood()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Wrong kind ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{}))
	// nil audit ignored.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	// Wrong action ignored.
	a.OnEvent(rt, auditEvent(now, &mq.AdminAuditMessage{Action: "admin.something.else"}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}
}

func TestLoginFailureFlood_GroupByIP(t *testing.T) {
	a := NewLoginFailureFlood()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	for range 25 {
		a.OnEvent(rt, auditEvent(now, &mq.AdminAuditMessage{
			Action:     "admin.login.failed",
			SourceIP:   "10.0.0.1",
			ActorLabel: "a@b.com",
		}))
	}
	decisions := a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 20, "groupBy": "ip"}, now)
	// Should match the per-ip target only.
	var ipFired bool
	for _, d := range decisions {
		if d.Action == alerteval.Fire && d.TargetKey == "login:ip:10.0.0.1" {
			ipFired = true
		}
		if d.TargetKey == "login:all" {
			t.Errorf("groupBy=ip should not fire on login:all; got %+v", d)
		}
	}
	if !ipFired {
		t.Error("expected fire on login:ip:10.0.0.1")
	}
}

func TestLoginFailureFlood_GroupByEmail(t *testing.T) {
	a := NewLoginFailureFlood()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	for range 30 {
		a.OnEvent(rt, auditEvent(now, &mq.AdminAuditMessage{
			Action:     "admin.login.failed",
			ActorLabel: "victim@b.com",
		}))
	}
	decisions := a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 20, "groupBy": "email"}, now)
	var emailFired bool
	for _, d := range decisions {
		if d.Action == alerteval.Fire && d.TargetKey == "login:email:victim@b.com" {
			emailFired = true
		}
	}
	if !emailFired {
		t.Errorf("expected email fire, got %+v", decisions)
	}
}

func TestLoginFailureFlood_GroupByAll(t *testing.T) {
	a := NewLoginFailureFlood()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	for range 25 {
		a.OnEvent(rt, auditEvent(now, &mq.AdminAuditMessage{
			Action:     "admin.login.failed",
			SourceIP:   "1.1.1.1",
			ActorLabel: "a@b.com",
		}))
	}
	decisions := a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 20, "groupBy": "all"}, now)
	var allFired bool
	for _, d := range decisions {
		if d.Action == alerteval.Fire && d.TargetKey == "login:all" {
			allFired = true
		}
	}
	if !allFired {
		t.Errorf("expected groupBy=all fire on login:all; got %+v", decisions)
	}
}

func TestModelRateLimitedResponses_Metadata(t *testing.T) {
	a := NewModelRateLimitedResponses()
	if a.RuleID() != "model.rate_limited_responses" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestModelRateLimitedResponses_OnEventAndFire(t *testing.T) {
	a := NewModelRateLimitedResponses()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Non-traffic / nil-traffic
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// non-429
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{StatusCode: intPtr(200)}))
	// 429 but Nexus-classified (errorCode set) ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		StatusCode: intPtr(429), ErrorCode: strPtr("RATE_LIMITED"),
	}))
	// 429 but no model
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{StatusCode: intPtr(429)}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets after filtering, got %v", rt.Targets())
	}

	// 15 upstream 429s for one routed model.
	for range 15 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			StatusCode:    intPtr(429),
			RoutedModelID: strPtr("gpt-4o"),
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 10}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "model:gpt-4o" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestModelRateLimitedResponses_FallbackModelID(t *testing.T) {
	a := NewModelRateLimitedResponses()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		StatusCode: intPtr(429),
		ModelID:    strPtr("claude-3"), // fallback because RoutedModelID is nil
	}))
	targets := rt.Targets()
	if len(targets) != 1 || targets[0] != "model:claude-3" {
		t.Errorf("expected fallback to ModelID, got %v", targets)
	}
}

func TestProviderHighLatencyPercentile_Metadata(t *testing.T) {
	a := NewProviderHighLatencyPercentile()
	if a.RuleID() != "provider.high_latency_percentile" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 3900 {
		t.Errorf("MinWarmupSec default got %d", a.MinWarmupSec(nil))
	}
}

func TestProviderHighLatencyPercentile_FilteringAndFire(t *testing.T) {
	a := NewProviderHighLatencyPercentile()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// no latency
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		LatencyMs: intPtr(0), ProviderID: strPtr("p"),
	}))
	// no provider
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{LatencyMs: intPtr(500)}))

	if len(rt.SampleTargets()) != 0 {
		t.Errorf("expected no sample targets, got %v", rt.SampleTargets())
	}

	// Baseline samples — 30min ago — low latency 500ms. Then alert
	// samples at 8000ms in last 300s. Need alertN >= minSamples
	// (default 50). samplesCap=1000 hardcoded by production; pick
	// 950 baseline + 50 alert. Nearest-rank p95 over 1000 sorted
	// samples = index int(0.95 * 999) = 949 → still baseline (indices
	// 0..949 = 500; 950..999 = 8000). basePct=500. alertPct over
	// 300s lookback = 8000. 8000 > 1.5*500. Fire.
	for range 950 {
		a.OnEvent(rt, trafficEvent(now.Add(-30*time.Minute), &consumer.TrafficEventMessage{
			LatencyMs:        intPtr(500),
			RoutedProviderID: strPtr("openai"),
		}))
	}
	for i := range 50 {
		a.OnEvent(rt, trafficEvent(now.Add(-time.Duration(i)*time.Second), &consumer.TrafficEventMessage{
			LatencyMs:        intPtr(8000),
			RoutedProviderID: strPtr("openai"),
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{"multiplier": 1.5}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "provider:openai" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestProviderHighLatencyPercentile_FallbackProviderID(t *testing.T) {
	a := NewProviderHighLatencyPercentile()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		LatencyMs:  intPtr(500),
		ProviderID: strPtr("anthropic"), // fallback path
	}))
	targets := rt.SampleTargets()
	if len(targets) != 1 || targets[0] != "provider:anthropic" {
		t.Errorf("expected provider:anthropic fallback, got %v", targets)
	}
}

func TestProviderUpstreamError_Metadata(t *testing.T) {
	a := NewProviderUpstreamError()
	if a.RuleID() != "provider.upstream_error" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestProviderUpstreamError_FilteringAndFire(t *testing.T) {
	a := NewProviderUpstreamError()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Filtering branches.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{StatusCode: intPtr(500)})) // no provider

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	// 50 ok responses.
	for range 50 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			StatusCode:       intPtr(200),
			RoutedProviderID: strPtr("openai"),
		}))
	}
	// 10 upstream 500s.
	for range 10 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			StatusCode:       intPtr(503),
			RoutedProviderID: strPtr("openai"),
		}))
	}
	// 5 Nexus-classified 5xx (not counted as numerator since errorCode set).
	for range 5 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			StatusCode:       intPtr(503),
			ErrorCode:        strPtr("HOOK_REJECT"),
			RoutedProviderID: strPtr("openai"),
		}))
	}
	// 10/65 ≈ 15% > 10%
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdPct": 10, "minSamples": 20}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "provider:openai" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestProviderUpstreamError_FallbackProviderID(t *testing.T) {
	a := NewProviderUpstreamError()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		StatusCode: intPtr(200),
		ProviderID: strPtr("anthropic"),
	}))
	if got := rt.Targets(); len(got) != 1 || got[0] != "provider:anthropic" {
		t.Errorf("expected provider:anthropic, got %v", got)
	}
}

func TestProxyCostSpike_Metadata(t *testing.T) {
	a := NewProxyCostSpike()
	if a.RuleID() != "proxy.cost_spike" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 3600 {
		t.Error("MinWarmupSec default")
	}
}

func TestProxyCostSpike_FilteringAndFire(t *testing.T) {
	a := NewProxyCostSpike()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Filter branches.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Non-vk entity ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("user"), EntityID: strPtr("u1"), EstimatedCostUSD: floatPtr(1.0),
	}))
	// vk with empty id ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), EstimatedCostUSD: floatPtr(1.0),
	}))
	// Zero cost ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), EntityID: strPtr("v1"),
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets after filtering, got %v", rt.Targets())
	}

	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), EntityID: strPtr("v1"), EstimatedCostUSD: floatPtr(150.0),
	}))
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 3600, "thresholdUsd": 100.0}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "vk:v1" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestProxyHighErrorRate_Metadata(t *testing.T) {
	a := NewProxyHighErrorRate()
	if a.RuleID() != "proxy.high_error_rate" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 3 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestProxyHighErrorRate_FilteringAndFire(t *testing.T) {
	a := NewProxyHighErrorRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Empty thingKey ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{StatusCode: intPtr(500)}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	for range 20 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess: strPtr("svc-a"), StatusCode: intPtr(200),
		}))
	}
	for range 5 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess: strPtr("svc-a"), StatusCode: intPtr(500),
		}))
	}
	// 5/25 = 20% > 10%
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdPct": 10, "minSamples": 10}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "thing:svc-a" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestProxyHighErrorRate_FallbackThingKeyFromSource(t *testing.T) {
	a := NewProxyHighErrorRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		Source: "compliance-proxy", StatusCode: intPtr(200),
	}))
	if got := rt.Targets(); len(got) != 1 || got[0] != "thing:compliance-proxy" {
		t.Errorf("expected fallback, got %v", got)
	}
}

// ProxyHookFailureRate + walkHooks + isTimeoutErr + ProxyHookTimeoutRate

func TestWalkHooks_VariousInputs(t *testing.T) {
	// Nil + empty + malformed.
	if a, b, c := walkHooks(nil, []byte("")); a || b || c {
		t.Errorf("nil+empty: %v %v %v", a, b, c)
	}
	if a, b, c := walkHooks([]byte("bad"), json.RawMessage("[bad")); a || b || c {
		t.Errorf("malformed should be silent: %v %v %v", a, b, c)
	}
	// default branch (int)
	if a, b, c := walkHooks(42, "string"); a || b || c {
		t.Errorf("unknown types: %v %v %v", a, b, c)
	}

	// One successful hook + one failing + one timeout.
	req := []byte(`[{"stage":"r1","error":""},{"stage":"r2","error":"some failure"}]`)
	resp := json.RawMessage(`[{"stage":"s1","error":"deadline exceeded"}]`)
	hasAny, hasFail, hasTO := walkHooks(req, resp)
	if !hasAny {
		t.Error("expected hasAnyHook")
	}
	if !hasFail {
		t.Error("expected hasFailure")
	}
	if !hasTO {
		t.Error("expected hasTimeout")
	}

	// Only successful hooks.
	clean := []byte(`[{"stage":"a","error":""}]`)
	hasAny2, hasFail2, hasTO2 := walkHooks(clean, nil)
	if !hasAny2 || hasFail2 || hasTO2 {
		t.Errorf("clean: %v %v %v", hasAny2, hasFail2, hasTO2)
	}
}

func TestIsTimeoutErr(t *testing.T) {
	cases := map[string]bool{
		"context deadline exceeded": true,
		"DEADLINE EXCEEDED":         true,
		"Timeout occurred":          true,
		"some other failure":        false,
		"":                          false,
	}
	for s, want := range cases {
		if got := isTimeoutErr(s); got != want {
			t.Errorf("isTimeoutErr(%q)=%v, want %v", s, got, want)
		}
	}
}

func TestProxyHookFailureRate_Metadata(t *testing.T) {
	a := NewProxyHookFailureRate()
	if a.RuleID() != "proxy.hook_failure_rate" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 3 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestProxyHookFailureRate_OnEventAndFire(t *testing.T) {
	a := NewProxyHookFailureRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// No hooks → not counted.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{SourceProcess: strPtr("svc")}))
	// Has hooks but empty thingKey ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		RequestHooksPipeline: json.RawMessage(`[{"stage":"a","error":""}]`),
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	clean := json.RawMessage(`[{"stage":"a","error":""}]`)
	failing := json.RawMessage(`[{"stage":"a","error":"x failed"}]`)
	for range 20 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess: strPtr("svc-a"), RequestHooksPipeline: clean,
		}))
	}
	for range 10 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess: strPtr("svc-a"), RequestHooksPipeline: failing,
		}))
	}
	// 10/30 = 33% > 20%
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdPct": 20, "minSamples": 10}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}

func TestProxyHookFailureRate_FallbackThingKey(t *testing.T) {
	a := NewProxyHookFailureRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		Source:               "compliance-proxy",
		RequestHooksPipeline: json.RawMessage(`[{"stage":"a","error":""}]`),
	}))
	if got := rt.Targets(); len(got) != 1 || got[0] != "thing:compliance-proxy" {
		t.Errorf("expected fallback, got %v", got)
	}
}

func TestProxyHookTimeoutRate_Metadata(t *testing.T) {
	a := NewProxyHookTimeoutRate()
	if a.RuleID() != "proxy.hook_timeout_rate" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 3 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestProxyHookTimeoutRate_OnEventAndFire(t *testing.T) {
	a := NewProxyHookTimeoutRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// No hooks → not counted.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{SourceProcess: strPtr("svc")}))
	// Has hooks but empty thingKey ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		RequestHooksPipeline: json.RawMessage(`[{"stage":"a","error":"deadline exceeded"}]`),
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	clean := json.RawMessage(`[{"stage":"a","error":""}]`)
	timing := json.RawMessage(`[{"stage":"a","error":"context deadline exceeded"}]`)
	for range 30 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess: strPtr("svc-a"), RequestHooksPipeline: clean,
		}))
	}
	for range 5 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			SourceProcess: strPtr("svc-a"), RequestHooksPipeline: timing,
		}))
	}
	// 5/35 ≈ 14% > 10%
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdPct": 10, "minSamples": 10}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
}

func TestProxyHookTimeoutRate_FallbackThingKey(t *testing.T) {
	a := NewProxyHookTimeoutRate()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		Source:               "agent",
		RequestHooksPipeline: json.RawMessage(`[{"stage":"a","error":""}]`),
	}))
	if got := rt.Targets(); len(got) != 1 || got[0] != "thing:agent" {
		t.Errorf("expected fallback, got %v", got)
	}
}

func TestProxyQuotaRuntimeExceeded_Metadata(t *testing.T) {
	a := NewProxyQuotaRuntimeExceeded()
	if a.RuleID() != "proxy.quota_runtime_exceeded" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestProxyQuotaRuntimeExceeded_OnEventAndFire(t *testing.T) {
	a := NewProxyQuotaRuntimeExceeded()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Other error code ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		ErrorCode: strPtr("RATE_LIMITED"), EntityType: strPtr("vk"), EntityID: strPtr("v"),
	}))
	// Non-vk entity ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		ErrorCode: strPtr("QUOTA_EXCEEDED"), EntityType: strPtr("user"), EntityID: strPtr("u"),
	}))
	// Vk with empty id ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		ErrorCode: strPtr("QUOTA_EXCEEDED"), EntityType: strPtr("vk"),
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	for range 15 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			ErrorCode: strPtr("QUOTA_EXCEEDED"), EntityType: strPtr("vk"), EntityID: strPtr("vk-1"),
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 10}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "vk:vk-1" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestProxyRateLimitExceeded_Metadata(t *testing.T) {
	a := NewProxyRateLimitExceeded()
	if a.RuleID() != "proxy.rate_limit_exceeded" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 300 {
		t.Error("MinWarmupSec default")
	}
}

func TestProxyRateLimitExceeded_GroupByVK(t *testing.T) {
	a := NewProxyRateLimitExceeded()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	// Filtering branches.
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{ErrorCode: strPtr("OTHER")}))

	for range 35 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			ErrorCode:  strPtr("RATE_LIMITED"),
			EntityType: strPtr("vk"),
			EntityID:   strPtr("v1"),
			SourceIP:   strPtr("1.2.3.4"),
		}))
	}
	decisions := a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 30, "groupBy": "vk"}, now)
	var vkFired bool
	for _, d := range decisions {
		if d.Action == alerteval.Fire && d.TargetKey == "rl:vk:v1" {
			vkFired = true
		}
		if d.TargetKey == "rl:all" {
			t.Errorf("groupBy=vk should not fire on rl:all; got %+v", d)
		}
	}
	if !vkFired {
		t.Error("expected fire on rl:vk:v1")
	}
}

func TestProxyRateLimitExceeded_GroupByIP(t *testing.T) {
	a := NewProxyRateLimitExceeded()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	for range 35 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			ErrorCode: strPtr("RATE_LIMITED"),
			SourceIP:  strPtr("1.2.3.4"),
		}))
	}
	decisions := a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 30, "groupBy": "ip"}, now)
	var ipFired bool
	for _, d := range decisions {
		if d.Action == alerteval.Fire && d.TargetKey == "rl:ip:1.2.3.4" {
			ipFired = true
		}
	}
	if !ipFired {
		t.Errorf("expected ip fire, got %+v", decisions)
	}
}

func TestProxyRateLimitExceeded_GroupByAll(t *testing.T) {
	a := NewProxyRateLimitExceeded()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()
	for range 35 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			ErrorCode: strPtr("RATE_LIMITED"),
		}))
	}
	decisions := a.Tick(rt, map[string]any{"windowSec": 300, "thresholdCount": 30, "groupBy": "all"}, now)
	var allFired bool
	for _, d := range decisions {
		if d.Action == alerteval.Fire && d.TargetKey == "rl:all" {
			allFired = true
		}
	}
	if !allFired {
		t.Errorf("expected fire on rl:all, got %+v", decisions)
	}
}

func TestProxyRoutingNoMatch_Metadata(t *testing.T) {
	a := NewProxyRoutingNoMatch()
	if a.RuleID() != "proxy.routing_no_match" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 600 {
		t.Error("MinWarmupSec default")
	}
}

func TestProxyRoutingNoMatch_FilteringAndFire(t *testing.T) {
	a := NewProxyRoutingNoMatch()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Wrong error code ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{ErrorCode: strPtr("OTHER")}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	for range 25 {
		a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
			ErrorCode: strPtr("ROUTING_NO_MATCH"),
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 600, "thresholdCount": 20}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "global" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestVKLatencyDegradation_Metadata(t *testing.T) {
	a := NewVKLatencyDegradation()
	if a.RuleID() != "vk.latency_degradation" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 3900 {
		t.Error("MinWarmupSec default")
	}
}

func TestVKLatencyDegradation_FilteringAndFire(t *testing.T) {
	a := NewVKLatencyDegradation()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Non-vk entity ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("user"), EntityID: strPtr("u"), LatencyMs: intPtr(100),
	}))
	// vk with empty id ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), LatencyMs: intPtr(100),
	}))
	// Zero latency ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), EntityID: strPtr("v"), LatencyMs: intPtr(0),
	}))

	if len(rt.SampleTargets()) != 0 {
		t.Errorf("expected no sample targets, got %v", rt.SampleTargets())
	}

	// Baseline + alert burst — see provider test for the math.
	for range 950 {
		a.OnEvent(rt, trafficEvent(now.Add(-30*time.Minute), &consumer.TrafficEventMessage{
			EntityType: strPtr("vk"), EntityID: strPtr("vk-x"), LatencyMs: intPtr(500),
		}))
	}
	for i := range 50 {
		a.OnEvent(rt, trafficEvent(now.Add(-time.Duration(i)*time.Second), &consumer.TrafficEventMessage{
			EntityType: strPtr("vk"), EntityID: strPtr("vk-x"), LatencyMs: intPtr(8000),
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{"multiplier": 1.5}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "vk:vk-x" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestVKTokenUsageSpike_Metadata(t *testing.T) {
	a := NewVKTokenUsageSpike()
	if a.RuleID() != "vk.token_usage_spike" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 1 {
		t.Errorf("Sources: %v", a.Sources())
	}
	if a.MinWarmupSec(nil) != 3600 {
		t.Error("MinWarmupSec default")
	}
}

func TestVKTokenUsageSpike_FilteringAndFire(t *testing.T) {
	a := NewVKTokenUsageSpike()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Non-vk entity ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("user"), EntityID: strPtr("u"), TotalTokens: intPtr(100),
	}))
	// vk with empty id ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), TotalTokens: intPtr(100),
	}))
	// Zero tokens ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), EntityID: strPtr("v"),
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"), EntityID: strPtr("vk-1"), TotalTokens: intPtr(1500000),
	}))
	d := fireFromTick(a.Tick(rt, map[string]any{"windowSec": 3600, "thresholdTokens": 1000000}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "vk:vk-1" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}

func TestVKTrafficSpike_Metadata(t *testing.T) {
	a := NewVKTrafficSpike()
	if a.RuleID() != "vk.traffic_spike" {
		t.Errorf("RuleID: %s", a.RuleID())
	}
	if len(a.Sources()) != 3 {
		t.Errorf("Sources: %v", a.Sources())
	}
	// Default cs=24 hours.
	if got := a.MinWarmupSec(nil); got != 24*3600 {
		t.Errorf("MinWarmupSec default: %d", got)
	}
	// cs=0 → falls back to baseline + alert.
	if got := a.MinWarmupSec(map[string]any{"coldStartHours": 0}); got != 3900 {
		t.Errorf("MinWarmupSec cs=0: %d", got)
	}
	// cs set.
	if got := a.MinWarmupSec(map[string]any{"coldStartHours": 2}); got != 7200 {
		t.Errorf("MinWarmupSec cs=2: %d", got)
	}
}

func TestVKTrafficSpike_FilteringAndFire(t *testing.T) {
	a := NewVKTrafficSpike()
	rt := alerteval.NewRuntime(a.RuleID(), time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventAudit, Timestamp: now})
	a.OnEvent(rt, &alerteval.Event{Kind: alerteval.EventTraffic, Timestamp: now})
	// Non-vk entity ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("user"), EntityID: strPtr("u"),
	}))
	// vk empty id ignored.
	a.OnEvent(rt, trafficEvent(now, &consumer.TrafficEventMessage{
		EntityType: strPtr("vk"),
	}))

	if len(rt.Targets()) != 0 {
		t.Errorf("expected no targets, got %v", rt.Targets())
	}

	// Baseline: some quiet traffic spread over the past hour.
	for i := range 12 {
		a.OnEvent(rt, trafficEvent(now.Add(-time.Duration(i*5+10)*time.Minute), &consumer.TrafficEventMessage{
			EntityType: strPtr("vk"), EntityID: strPtr("vk-burst"),
		}))
	}
	// Burst in last 5 min: 200 events.
	for i := range 200 {
		a.OnEvent(rt, trafficEvent(now.Add(-time.Duration(i)*time.Second), &consumer.TrafficEventMessage{
			EntityType: strPtr("vk"), EntityID: strPtr("vk-burst"),
		}))
	}
	d := fireFromTick(a.Tick(rt, map[string]any{
		"alertWindowSec":    300,
		"baselineWindowSec": 3600,
		"spikeMultiplier":   10.0,
		"absFloorReq":       50,
	}, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("expected Fire, got %+v", d)
	}
	if d.TargetKey != "vk:vk-burst" {
		t.Errorf("unexpected target: %s", d.TargetKey)
	}
}
