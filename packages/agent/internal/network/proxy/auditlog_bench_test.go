package proxy

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// loggingQueueWriter.Enqueue runs once per intercepted request on END-USER
// hardware. Unlike a server, the agent has no admission control in front of it
// and the user pays for every log write in CPU wakeups, disk I/O and battery.

type nopWriter struct{ n int }

func (w *nopWriter) Enqueue(sharedaudit.AuditEvent) { w.n++ }
func (w *nopWriter) Flush(context.Context) error    { return nil }
func (w *nopWriter) Close(context.Context) error    { return nil }

func benchEvent() sharedaudit.AuditEvent {
	return sharedaudit.AuditEvent{
		ID:                  "evt_01HQ8Z3K7M2N4P6R8T0V2X4Y6A",
		TraceID:             "9f2c1e77-3b4a-4d5e-8f6a-7b8c9d0e1f20",
		TargetHost:          "api.openai.com",
		Method:              "POST",
		Path:                "/v1/chat/completions",
		RequestHookDecision: "APPROVE",
		BumpStatus:          "BUMP_SUCCESS",
		DomainRuleID:        "dom_openai_prod",
		PathAction:          "PROCESS",
		SourceProcess:       "Google Chrome Helper",
		SourceProcessBundle: "com.google.Chrome.helper",
		Provider:            "openai",
		Model:               "gpt-4o",
		LatencyMs:           842,
	}
}

// benchEnqueue measures the wrapper at a REAL handler (not io.Discard alone —
// the point is the marshal + write cost that a laptop's disk actually pays, so
// the text handler must run) with INFO enabled, which is the production level.
func benchEnqueue(b *testing.B, out io.Writer) {
	w := &loggingQueueWriter{
		next:   &nopWriter{},
		logger: slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	e := benchEvent()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		w.Enqueue(e)
	}
}

// BenchmarkAuditEnqueue_Log measures the per-request cost with the handler
// formatting into a discarded buffer.
func BenchmarkAuditEnqueue_Log(b *testing.B) { benchEnqueue(b, io.Discard) }

// TestAuditEnqueue_LogCarriesCorrelationIDs pins the diagnostic contract the
// line exists for: exactly one INFO entry per request, correlatable by event id
// AND trace id. Whatever else is trimmed for cost, these two must survive —
// they are what makes the line joinable to the audit row that holds the rest.
func TestAuditEnqueue_LogCarriesCorrelationIDs(t *testing.T) {
	var buf strings.Builder
	next := &nopWriter{}
	w := &loggingQueueWriter{
		next:   next,
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	e := benchEvent()
	w.Enqueue(e)

	got := buf.String()
	if n := strings.Count(got, "audit emit (per-request)"); n != 1 {
		t.Fatalf("expected exactly one anchor line per request, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, e.ID) {
		t.Errorf("event_id missing from the anchor line — the row cannot be joined:\n%s", got)
	}
	if !strings.Contains(got, e.TraceID) {
		t.Errorf("trace_id missing from the anchor line — cross-service correlation is lost:\n%s", got)
	}
	// The wrapper must still forward to the real writer; a diagnostic change
	// must never drop the audit row itself.
	if next.n != 1 {
		t.Errorf("audit event was not forwarded to the queue writer (n=%d)", next.n)
	}
}

// TestAuditEnqueue_NoDuplicatedRowColumns pins the cost decision: the fields
// that are columns on the audit row must NOT be re-logged. If someone adds one
// back, this fails and forces the trade-off to be made deliberately.
func TestAuditEnqueue_NoDuplicatedRowColumns(t *testing.T) {
	var buf strings.Builder
	w := &loggingQueueWriter{
		next:   &nopWriter{},
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	w.Enqueue(benchEvent())

	got := buf.String()
	for _, key := range []string{
		"target_host", "method=", "path=", "hook_decision", "bump_status",
		"domain_rule_id", "path_action", "source_process", "source_bundle",
		"provider", "model", "latency_ms",
	} {
		if strings.Contains(got, key) {
			t.Errorf("attribute %q is a column on the audit row and must not be duplicated into the per-request log line:\n%s", key, got)
		}
	}
}
