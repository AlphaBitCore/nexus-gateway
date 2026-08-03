package wiring

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
)

func newWiringTestQueue(t *testing.T) *auditqueue.Queue {
	t.Helper()
	q, err := auditqueue.NewQueue(":memory:", nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// TestNewBridgeAuditWriter_NilQueueYieldsNilWriter — a missing queue must NOT
// produce a writer that silently swallows audit. BumpFlow's nil-sink guard is
// what turns this into a loud failure, and it can only fire on a nil Writer.
func TestNewBridgeAuditWriter_NilQueueYieldsNilWriter(t *testing.T) {
	if w := NewBridgeAuditWriter(nil, "spill", slog.Default()); w != nil {
		t.Fatalf("nil queue must yield a nil Writer, got %T", w)
	}
}

// TestNewBridgeAuditWriter_ResolvesAndAnnouncesMode pins both halves of what
// this constructor is for: the configured mode actually lands on the writer, and
// the resolved mode is visible to whoever runs the agent. The gateway and the
// proxy both log this; the agent's shipped default DIVERGES from the shared
// no-loss default, which makes the announcement more load-bearing here, not less.
func TestNewBridgeAuditWriter_ResolvesAndAnnouncesMode(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		want       lossmode.Mode
	}{
		{"shipped agent default", "spill", lossmode.Spill},
		{"explicit no-loss", "spillblock", lossmode.SpillBlock},
		{"empty falls back to the shared no-loss default", "", lossmode.Default},
		{"a typo must never resolve to a lossier mode", "spil", lossmode.Default},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			w := NewBridgeAuditWriter(newWiringTestQueue(t), tc.configured, logger)
			if w == nil {
				t.Fatal("want a writer for a non-nil queue")
			}
			t.Cleanup(func() { _ = w.Close(context.Background()) })

			qw, ok := w.(*auditqueue.QueueWriter)
			if !ok {
				t.Fatalf("want *auditqueue.QueueWriter, got %T", w)
			}
			if got := qw.LossMode(); got != tc.want {
				t.Errorf("resolved loss mode = %q, want %q", got, tc.want)
			}

			var line struct {
				Msg        string `json:"msg"`
				Configured string `json:"configured"`
				Resolved   string `json:"resolved"`
			}
			raw := strings.TrimSpace(buf.String())
			if raw == "" {
				t.Fatal("wiring the audit writer logged nothing — the resolved mode is invisible to the operator")
			}
			if err := json.Unmarshal([]byte(raw), &line); err != nil {
				t.Fatalf("parse log line %q: %v", raw, err)
			}
			if line.Configured != tc.configured {
				t.Errorf("log configured = %q, want %q", line.Configured, tc.configured)
			}
			if line.Resolved != string(tc.want) {
				t.Errorf("log resolved = %q, want %q", line.Resolved, tc.want)
			}
		})
	}
}
