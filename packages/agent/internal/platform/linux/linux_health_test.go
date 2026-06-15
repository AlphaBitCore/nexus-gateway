//go:build linux

package linux

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestReconcilerHealth_RecordSuccess pins that a successful tick marks the
// reconciler installed, clears the failure counter and last error, and
// stamps LastReconcileAt — the baseline "chain is being maintained" state.
func TestReconcilerHealth_RecordSuccess(t *testing.T) {
	r := NewReconciler(slog.Default(), 19080)
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	r.recordHealth(now, nil)

	h := r.Health()
	if !h.Installed {
		t.Error("after a successful tick, Installed must be true")
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", h.ConsecutiveFailures)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty", h.LastError)
	}
	if !h.LastReconcileAt.Equal(now) {
		t.Errorf("LastReconcileAt = %v, want %v", h.LastReconcileAt, now)
	}
	if reason := r.degradedReason(); reason != "" {
		t.Errorf("healthy reconciler must have empty degradedReason, got %q", reason)
	}
}

// TestReconcilerHealth_NotInstalled pins that before any successful tick the
// degraded reason is the actionable "not installed" message.
func TestReconcilerHealth_NotInstalled(t *testing.T) {
	r := NewReconciler(slog.Default(), 19080)
	if h := r.Health(); h.Installed {
		t.Error("fresh reconciler must not report Installed")
	}
	if got, want := r.degradedReason(), "iptables redirect chain not installed"; got != want {
		t.Errorf("degradedReason = %q, want %q", got, want)
	}
}

// TestReconcilerHealth_TransientFailureTolerated pins that a SINGLE failed
// tick after a successful install does NOT degrade — that is just transient
// xtables-lock contention. The chain stays Installed and the reason is empty.
func TestReconcilerHealth_TransientFailureTolerated(t *testing.T) {
	r := NewReconciler(slog.Default(), 19080)
	r.recordHealth(time.Now(), nil)                             // install
	r.recordHealth(time.Now(), errors.New("xtables lock busy")) // one transient failure

	h := r.Health()
	if !h.Installed {
		t.Error("Installed must remain true after a single transient failure")
	}
	if h.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", h.ConsecutiveFailures)
	}
	if reason := r.degradedReason(); reason != "" {
		t.Errorf("single failure must be tolerated; got degradedReason %q", reason)
	}
}

// TestReconcilerHealth_PersistentFailureDegrades pins that reaching the
// failure threshold surfaces an actionable degraded reason that includes the
// failure count and the underlying error text.
func TestReconcilerHealth_PersistentFailureDegrades(t *testing.T) {
	r := NewReconciler(slog.Default(), 19080)
	r.recordHealth(time.Now(), nil) // install first
	for range maxReconcileFailuresBeforeDegraded {
		r.recordHealth(time.Now(), errors.New("permission denied"))
	}

	reason := r.degradedReason()
	if reason == "" {
		t.Fatal("persistent failures must produce a degraded reason")
	}
	if !strings.Contains(reason, "repair failing") {
		t.Errorf("reason should mention repair failing, got %q", reason)
	}
	if !strings.Contains(reason, "permission denied") {
		t.Errorf("reason should surface the underlying error, got %q", reason)
	}
}

// TestReconcilerHealth_RecoveryResetsCounter pins that a success after
// failures clears the counter, so a flushed-then-repaired chain (the normal
// firewalld-reload case) returns to healthy instead of staying degraded.
func TestReconcilerHealth_RecoveryResetsCounter(t *testing.T) {
	r := NewReconciler(slog.Default(), 19080)
	r.recordHealth(time.Now(), nil)
	for range maxReconcileFailuresBeforeDegraded {
		r.recordHealth(time.Now(), errors.New("boom"))
	}
	if r.degradedReason() == "" {
		t.Fatal("precondition: should be degraded after threshold failures")
	}
	r.recordHealth(time.Now(), nil) // repair succeeds
	if got := r.Health().ConsecutiveFailures; got != 0 {
		t.Errorf("ConsecutiveFailures = %d after recovery, want 0", got)
	}
	if reason := r.degradedReason(); reason != "" {
		t.Errorf("after recovery degradedReason must be empty, got %q", reason)
	}
}

// TestInterceptionHealth_NilReconciler pins the pre-Start / failed-install
// case: no reconciler pointer means nothing is maintaining the chain, so the
// platform reports degraded + not connected and still flags SelfReported so
// the collector trusts the verdict.
func TestInterceptionHealth_NilReconciler(t *testing.T) {
	p := &LinuxPlatform{}
	h := p.InterceptionHealth()
	if !h.SelfReported {
		t.Error("Linux InterceptionHealth must set SelfReported")
	}
	if h.Connected {
		t.Error("nil reconciler must report Connected=false")
	}
	if h.DegradedReason != "iptables redirect chain not installed" {
		t.Errorf("DegradedReason = %q, want not-installed message", h.DegradedReason)
	}
}

// TestInterceptionHealth_IdleHealthy pins the critical anti-regression: an
// enrolled host with a healthy reconciler and ZERO flows is Connected and
// has no degraded reason. This is the case the generic ConnectionsTotal==0
// heuristic would wrongly flag.
func TestInterceptionHealth_IdleHealthy(t *testing.T) {
	rec := NewReconciler(slog.Default(), 19080)
	rec.recordHealth(time.Now(), nil) // installed, healthy

	p := &LinuxPlatform{}
	p.reconciler.Store(rec)
	p.startedAtNS.Store(time.Now().UnixNano())

	h := p.InterceptionHealth()
	if !h.Connected {
		t.Error("idle host with healthy reconciler must be Connected")
	}
	if h.DegradedReason != "" {
		t.Errorf("idle healthy host must have empty DegradedReason, got %q", h.DegradedReason)
	}
	if h.ConnectionsTotal != 0 {
		t.Errorf("ConnectionsTotal = %d, want 0 on idle host", h.ConnectionsTotal)
	}
}

// TestInterceptionHealth_CountersAndDegraded pins that flow counters are
// surfaced for diagnostics AND that a persistently-failing reconciler drives
// the degraded verdict even though flows have been seen.
func TestInterceptionHealth_CountersAndDegraded(t *testing.T) {
	rec := NewReconciler(slog.Default(), 19080)
	rec.recordHealth(time.Now(), nil)
	for range maxReconcileFailuresBeforeDegraded {
		rec.recordHealth(time.Now(), errors.New("lost CAP_NET_ADMIN"))
	}

	p := &LinuxPlatform{}
	p.reconciler.Store(rec)
	started := time.Now().Add(-time.Minute)
	p.startedAtNS.Store(started.UnixNano())
	p.connectionsTotal.Store(42)
	p.activeSessions.Store(3)
	lastFlow := time.Now().Add(-time.Second)
	p.lastFlowAtNS.Store(lastFlow.UnixNano())

	h := p.InterceptionHealth()
	if h.Connected {
		t.Error("failing reconciler must report Connected=false")
	}
	if !strings.Contains(h.DegradedReason, "repair failing") {
		t.Errorf("DegradedReason = %q, want repair-failing", h.DegradedReason)
	}
	if h.ConnectionsTotal != 42 || h.ActiveSessions != 3 {
		t.Errorf("counters not surfaced: total=%d active=%d", h.ConnectionsTotal, h.ActiveSessions)
	}
	if h.StartedAt.IsZero() || h.LastFlowAt.IsZero() {
		t.Error("StartedAt / LastFlowAt must be surfaced from the atomics")
	}
}

// TestMergeInterceptMs covers the latency-breakdown stamping helper.
func TestMergeInterceptMs(t *testing.T) {
	base := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

	// Zero interceptDoneAt → no key added, nil stays nil.
	if got := mergeInterceptMs(nil, base, time.Time{}); got != nil {
		t.Errorf("zero interceptDoneAt should return input unchanged, got %v", got)
	}

	// Normal case → creates map and stamps intercept_ms.
	got := mergeInterceptMs(nil, base, base.Add(50*time.Millisecond))
	if got["intercept_ms"] != 50 {
		t.Errorf("intercept_ms = %d, want 50", got["intercept_ms"])
	}

	// Negative delta clamps to 0.
	got = mergeInterceptMs(nil, base, base.Add(-10*time.Millisecond))
	if got["intercept_ms"] != 0 {
		t.Errorf("negative delta should clamp to 0, got %d", got["intercept_ms"])
	}

	// Existing breakdown is preserved.
	in := map[string]int{"upstream_ms": 7}
	got = mergeInterceptMs(in, base, base.Add(20*time.Millisecond))
	if got["upstream_ms"] != 7 || got["intercept_ms"] != 20 {
		t.Errorf("existing keys must be preserved, got %v", got)
	}
}
