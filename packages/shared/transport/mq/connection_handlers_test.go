package mq

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLogger returns a logger that writes JSON records to an in-memory
// buffer, plus a snapshot helper. Used to inspect what the natsmq
// connection handlers actually emit at each lifecycle event.
func captureLogger(t *testing.T) (*slog.Logger, func() string) {
	t.Helper()
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	h := slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(h)
	return logger, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// lockedWriter serialises concurrent writes from the watchdog goroutine
// and the synchronous handler calls so the test doesn't race on the
// buffer (Go's bytes.Buffer is not goroutine-safe).
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestWatchdogEscalatesOnSustainedDisconnect simulates a NATS outage that
// outlives the watchdog threshold and asserts the ERROR record is emitted.
func TestWatchdogEscalatesOnSustainedDisconnect(t *testing.T) {
	logger, snap := captureLogger(t)
	threshold := 50 * time.Millisecond

	onDisconnect, _, _, _ := newConnectionHandlers("producer", threshold, logger)
	onDisconnect(nil, nil) // simulate a NATS disconnect

	// Wait twice the threshold so the AfterFunc has had a chance to fire.
	time.Sleep(2 * threshold)

	logs := snap()
	if !strings.Contains(logs, `"natsmq: producer disconnected"`) {
		t.Errorf("missing disconnect WARN; got:\n%s", logs)
	}
	if !strings.Contains(logs, `"natsmq: producer still disconnected past threshold"`) {
		t.Errorf("watchdog ERROR did not fire after %v; got:\n%s", 2*threshold, logs)
	}
	if !strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("watchdog message did not log at ERROR level; got:\n%s", logs)
	}
}

// TestWatchdogCancelledOnFastReconnect simulates a normal NATS deploy:
// disconnect, then reconnect inside the threshold. The watchdog must NOT
// fire — that's the "deploy noise" the watchdog explicitly should suppress.
func TestWatchdogCancelledOnFastReconnect(t *testing.T) {
	logger, snap := captureLogger(t)
	threshold := 200 * time.Millisecond

	onDisconnect, onReconnect, _, _ := newConnectionHandlers("consumer", threshold, logger)
	onDisconnect(nil, nil)
	time.Sleep(20 * time.Millisecond) // fast reconnect well inside threshold
	onReconnect(nil)

	// Wait past threshold to be sure the cancelled timer doesn't fire.
	time.Sleep(2 * threshold)

	logs := snap()
	if !strings.Contains(logs, `"natsmq: consumer disconnected"`) {
		t.Errorf("missing disconnect WARN; got:\n%s", logs)
	}
	if !strings.Contains(logs, `"natsmq: consumer reconnected"`) {
		t.Errorf("missing reconnect INFO; got:\n%s", logs)
	}
	if strings.Contains(logs, "still disconnected past threshold") {
		t.Errorf("watchdog fired despite fast reconnect — cancel race regression; got:\n%s", logs)
	}
}

// TestWatchdogCancelledOnClosed verifies Closed also cancels the watchdog
// — terminal close is its own ERROR signal, we don't want a second
// redundant "still disconnected" ERROR firing a moment later.
func TestWatchdogCancelledOnClosedTerminal(t *testing.T) {
	logger, snap := captureLogger(t)
	threshold := 100 * time.Millisecond

	onDisconnect, _, onClosed, _ := newConnectionHandlers("producer", threshold, logger)
	onDisconnect(nil, nil)
	time.Sleep(10 * time.Millisecond)
	onClosed(nil) // c==nil is fine; the handler tolerates it

	time.Sleep(2 * threshold)

	logs := snap()
	if !strings.Contains(logs, `"natsmq: producer connection closed`) {
		t.Errorf("missing Closed ERROR; got:\n%s", logs)
	}
	if strings.Contains(logs, "still disconnected past threshold") {
		t.Errorf("watchdog fired after Closed cancelled it; got:\n%s", logs)
	}
}
