package audit

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
)

// Both overflow lines are per-event and can repeat for every request. Unthrottled,
// the ERROR one puts thousands of lines/second onto the same disk the NDJSON spool
// needs, at exactly the moment the box is saturated — and the WARN one floods to
// report the HEALTHY case, where the spool is working and nothing is lost. The
// Prometheus counters carry the true rate; the log carries a sample plus its total.
func TestOverflowLogs_AreThrottledAndCarryTheRunningTotal(t *testing.T) {
	var buf bytes.Buffer
	w := &MQBatchWriter{
		logger:   slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		lossMode: lossmode.Drop,
	}

	const n = overflowLogEvery * 2
	for range n {
		w.dropOverflow(AuditEvent{ID: "evt", TargetHost: "api.openai.com"}, "channel full", errors.New("nats down"))
	}

	out := buf.String()
	if got := strings.Count(out, "audit events DROPPED"); got != 2 {
		t.Errorf("logged %d times for %d drops; want 2 (one per %d)", got, n, overflowLogEvery)
	}
	// Without the total, a sampled log understates the loss by exactly the sampling
	// factor — which on an audit path is the number that matters.
	if !strings.Contains(out, "dropped_so_far=2001") {
		t.Errorf("the running total must be on the line:\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("a dropped audit record is an ERROR:\n%s", out)
	}
}
