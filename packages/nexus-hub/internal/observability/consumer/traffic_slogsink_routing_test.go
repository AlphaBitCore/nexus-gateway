package consumer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// recordingHandler is a slog.Handler that captures every Record passed to it
// without doing any I/O. Used to assert that a DI-injected logger truly
// dispatches to all installed handlers.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordingHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// wireMainLikeLogger mirrors what nexus-hub/cmd/nexus-hub/main.go does at
// startup:
//
//	jsonH := slog.NewJSONHandler(...)
//	slog.SetDefault(slog.New(jsonH))
//	hubDiagSink := shareddiag.NewSlogSink(...)
//	slog.SetDefault(slog.New(shareddiag.NewMultiHandler(logger.Handler(), hubDiagSink)))
//	logger = slog.Default()
//
// The critical last line is the DI-rebind: without it, downstream subsystems
// that received `logger` via constructor injection before SetDefault runs hold
// a snapshot pointing at the pre-SlogSink handler and bypass the recorder.
// This helper rebuilds the same chain in tests so callers can assert any
// child logger's Error() calls still light up the recorder.
func wireMainLikeLogger(t *testing.T) (*slog.Logger, *recordingHandler) {
	t.Helper()

	baseHandler := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(baseHandler))

	recorder := &recordingHandler{}
	slog.SetDefault(slog.New(shareddiag.NewMultiHandler(baseHandler, recorder)))
	logger := slog.Default()

	// The original slog.Default is restored after the test so parallel
	// tests using slog.Default don't see a polluted handler chain.
	originalDefault := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(originalDefault)
	})

	return logger, recorder
}

// TestTrafficEventWriterLoggerRoutesToSlogSink is the SlogSink-DI-bypass
// regression test. It mirrors main.go's logger wiring and then constructs a
// TrafficEventWriter the same way the production code does
// (consumer.NewTrafficEventWriter is the only call site). A trivial Error()
// on the writer's derived logger must land in the recording handler — if it
// doesn't, the DI rebind has regressed and Hub will once again silently lose
// flush errors during a prod incident.
//
// The test stays free of pgx — it's purely about the slog chain. A separate
// test below exercises the actual flush() error path against a closed pool.
func TestTrafficEventWriterLoggerRoutesToSlogSink(t *testing.T) {
	logger, recorder := wireMainLikeLogger(t)

	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, logger, nil)
	w.logger.Error("test flush error", "error", "fake")

	got := recorder.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorder saw %d records, want 1", len(got))
	}
	if got[0].Level != slog.LevelError {
		t.Errorf("record level = %v, want ERROR", got[0].Level)
	}
	if got[0].Message != "test flush error" {
		t.Errorf("record message = %q, want %q", got[0].Message, "test flush error")
	}
	// Note: the `.With("component", "traffic-event-writer")` attr is held on
	// the Handler chain (each handler's WithAttrs response), NOT on the
	// slog.Record itself. The production SlogSink intentionally returns `h`
	// from WithAttrs so it doesn't double-store attrs (the JSON sibling
	// handler renders them in its output). This recorder follows the same
	// convention, so we don't assert on `component` here — the main
	// invariant "ERROR reaches the SlogSink at all" is what catches the
	// DI-bypass class of regression.
}

// TestTrafficEventWriterFlushFailureReachesSlogSink stands up a real (but
// closed) pgxpool, exercises flush() against an empty batch so it fails at
// Begin, and asserts the resulting "flush: begin tx failed" Error record
// reaches the recording SlogSink. This is the end-to-end equivalent of the
// 16h prod gap: a real flush failure that should have surfaced on
// /infrastructure/errors but did not.
func TestTrafficEventWriterFlushFailureReachesSlogSink(t *testing.T) {
	// Non-destructive: we open a pool and close it immediately. The flush
	// call below fails at Begin() because the pool is already closed; no
	// SQL ever reaches the DB, no rows touched. The TEST_DATABASE_URL
	// requirement remains only because pgxpool.New still parses the DSN.
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("DB unavailable; skipping: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("DB ping failed; skipping: %v", err)
	}
	// Close immediately so the very first flush().Begin() fails. No SQL
	// runs against the live DB — this test is read-only-equivalent.
	pool.Close()

	logger, recorder := wireMainLikeLogger(t)

	w := NewTrafficEventWriter(pool, nil, TrafficEventWriterConfig{
		BatchSize:     1,
		FlushInterval: 100 * time.Millisecond,
	}, logger, nil)

	// One non-nil mq.Message with no-op Ack/Nak so nakAll() inside flush
	// doesn't panic when the batch is rejected for redelivery.
	noopMsg := &mq.Message{Ack: func() error { return nil }, Nak: func() error { return nil }}
	items := []pendingTrafficMessage{{event: TrafficEventMessage{}, msg: noopMsg}}

	if err := w.flush(context.Background(), items); err == nil {
		t.Fatalf("flush against closed pool: want error, got nil")
	}

	var sawBeginFail bool
	for _, r := range recorder.snapshot() {
		if r.Level == slog.LevelError && strings.Contains(r.Message, "flush: begin tx failed") {
			sawBeginFail = true
		}
	}
	if !sawBeginFail {
		t.Errorf("SlogSink recorder did not receive flush: begin tx failed ERROR after pool-closed flush; this is the SlogSink-DI bypass regression that caused the 16h prod audit gap on 2026-05-13/14")
	}
}
