package audit

// mem_budget_config_test.go — WithMemMaxBytes config semantics (mirrors
// NEXUS_EVENTS_MAX_BYTES): empty/"auto" keeps the auto-sized default, an
// explicit human size pins the budget, and an unparseable value keeps the
// default so a typo can never disable the OOM bound.

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestWithMemMaxBytes_Semantics(t *testing.T) {
	newW := func() *Writer {
		return NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Pin cases: an explicit size sets an exact, constant budget.
	pins := []struct {
		in   string
		want int64
	}{
		{"8GB", 8 << 30},        // human size pins
		{"2048MB", 2 << 30},     // MB suffix
		{"1073741824", 1 << 30}, // raw bytes
	}
	for _, c := range pins {
		if got := newW().WithMemMaxBytes(c.in).MemBudgetBytes(); got != c.want {
			t.Errorf("WithMemMaxBytes(%q) budget = %d, want %d", c.in, got, c.want)
		}
	}

	// No-op cases: unset / "auto" / unparseable must KEEP the writer's auto-sized
	// default. Compare each writer against ITS OWN pre-call budget, not a separately
	// captured sample: the auto value derives from live MemAvailable, which drifts
	// between reads on a busy host (Linux CI), so a fresh writer's default equals
	// only itself — a cross-writer comparison flakes by a few KB.
	for _, in := range []string{"", "auto", "AUTO", "garbage", "-5GB"} {
		w := newW()
		before := w.MemBudgetBytes()
		if before <= 0 {
			t.Fatalf("auto budget must be positive, got %d", before)
		}
		if got := w.WithMemMaxBytes(in).MemBudgetBytes(); got != before {
			t.Errorf("WithMemMaxBytes(%q) changed the auto budget %d → %d; want unchanged", in, before, got)
		}
	}
}

// A pinned budget must actually govern admission: a writer pinned to a tiny
// budget back-pressures exactly like a tiny auto budget would.
func TestWithMemMaxBytes_PinnedBudgetGovernsAdmission(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithMemMaxBytes("1KB")
	if !w.memBudget.TryAcquire(1024) {
		t.Fatal("first acquire within the pinned budget must admit")
	}
	if w.memBudget.TryAcquire(1) {
		t.Fatal("pinned 1KB budget must refuse once exhausted")
	}
}

// A pin above the safe share of available RAM must WARN (never clamp): the
// budget still applies verbatim — the operator stays in charge — but the
// OOM-risk signal must land in the log. Rig-validated failure this guards
// against: a 10GB pin on a 32GB box OOM-killed the gateway at 15.9GB RSS.
func TestWithMemMaxBytes_OversizedPinWarnsButApplies(t *testing.T) {
	withMeminfo(t, "MemAvailable:   4194304 kB\n") // 4 GiB available
	var buf strings.Builder
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(&buf, nil))).
		WithMemMaxBytes("2GB") // 50% of available — far past the 25% warn line
	if got := w.MemBudgetBytes(); got != 2<<30 {
		t.Fatalf("oversized pin must still apply verbatim, got %d", got)
	}
	if !strings.Contains(buf.String(), "OOM risk") {
		t.Fatalf("oversized pin must log the OOM-risk WARN; log was: %s", buf.String())
	}
	// A modest pin (below the warn line) stays silent.
	buf.Reset()
	NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(&buf, nil))).WithMemMaxBytes("512MB")
	if strings.Contains(buf.String(), "OOM risk") {
		t.Fatalf("modest pin must not warn; log was: %s", buf.String())
	}
}
