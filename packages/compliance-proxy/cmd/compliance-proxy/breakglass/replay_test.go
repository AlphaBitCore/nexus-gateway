package breakglass

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReplayer implements replayer for unit tests.
type fakeReplayer struct {
	mu      sync.Mutex
	drained bool
	err     error
	calls   int
}

func (f *fakeReplayer) ReplayPending(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.drained, f.err
}

func (f *fakeReplayer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// tickInterval is the short interval used in all replay loop tests so we
// don't wait 30 seconds for a real tick.
const tickInterval = 5 * time.Millisecond

// runReplayAndWait starts runReplayWith in a goroutine and returns a channel
// that closes once the loop exits. The caller is responsible for cancelling ctx.
func runReplayAndWait(ctx context.Context, srv replayer, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReplayWith(ctx, srv, logger, tickInterval)
	}()
	return done
}

// TestRunReplay_ExitsImmediatelyOnCancelledCtx exercises the public RunReplay
// wrapper (which delegates to runReplayWith with the package-level interval).
// Passing a pre-cancelled context means the loop selects ctx.Done()
// immediately and returns without ever calling srv.ReplayPending, so a nil
// *runtimeserver.Server is safe here.
func TestRunReplay_ExitsImmediatelyOnCancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before RunReplay starts

	done := make(chan struct{})
	go func() {
		defer close(done)
		// nil *runtimeserver.Server is safe: ctx is already done, so the
		// ticker.C branch (which calls srv.ReplayPending) never fires.
		RunReplay(ctx, nil, slog.Default())
	}()

	select {
	case <-done:
		// good: RunReplay exited without blocking
	case <-time.After(2 * time.Second):
		t.Fatal("RunReplay did not exit within 2s despite pre-cancelled context")
	}
}

// TestRunReplayWith_ExitsOnContextCancel verifies the drain loop returns
// cleanly when the context is cancelled, with no goroutine leak.
func TestRunReplayWith_ExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeReplayer{drained: false}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	done := runReplayAndWait(ctx, fake, logger)

	// Cancel after a short pause — the loop must exit promptly.
	cancel()
	select {
	case <-done:
		// good: loop exited
	case <-time.After(2 * time.Second):
		t.Fatal("runReplayWith did not exit after context cancellation within 2s")
	}
}

// TestRunReplayWith_LogsInfoOnDrained verifies that a successful drain
// (drained=true, err=nil) emits an Info-level log entry.
func TestRunReplayWith_LogsInfoOnDrained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fake := &fakeReplayer{drained: true}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	done := runReplayAndWait(ctx, fake, logger)

	// Wait for the fake to be called at least once, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.callCount() >= 1 {
			break
		}
		time.Sleep(tickInterval)
	}
	// Cancel and wait for the goroutine to fully exit before reading buf.
	cancel()
	<-done

	logOutput := buf.String()
	if !strings.Contains(logOutput, "INFO") || !strings.Contains(logOutput, "break-glass pending drained on tick") {
		t.Errorf("expected INFO log with drain message; got: %q", logOutput)
	}
}

// TestRunReplayWith_LogsWarnOnError verifies that a ReplayPending error
// emits a Warn-level log entry containing the error string.
func TestRunReplayWith_LogsWarnOnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	replayErr := errors.New("hub unavailable")
	fake := &fakeReplayer{drained: false, err: replayErr}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	done := runReplayAndWait(ctx, fake, logger)

	// Wait for at least one call, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.callCount() >= 1 {
			break
		}
		time.Sleep(tickInterval)
	}
	// Cancel and wait for the goroutine to fully exit before reading buf.
	cancel()
	<-done

	logOutput := buf.String()
	if !strings.Contains(logOutput, "WARN") || !strings.Contains(logOutput, "break-glass replay tick failed") {
		t.Errorf("expected WARN log with failure message; got: %q", logOutput)
	}
	if !strings.Contains(logOutput, "hub unavailable") {
		t.Errorf("expected error text 'hub unavailable' in log; got: %q", logOutput)
	}
}

// TestRunReplayWith_NoDrainLogWhenNotDrained verifies that no Info log is
// emitted when ReplayPending returns (false, nil) — nothing was pending.
func TestRunReplayWith_NoDrainLogWhenNotDrained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fake := &fakeReplayer{drained: false, err: nil}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	done := runReplayAndWait(ctx, fake, logger)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.callCount() >= 1 {
			break
		}
		time.Sleep(tickInterval)
	}
	// Cancel and wait for the goroutine to fully exit before reading buf.
	cancel()
	<-done

	logOutput := buf.String()
	if strings.Contains(logOutput, "break-glass pending drained on tick") {
		t.Errorf("expected no drain log when drained=false; got: %q", logOutput)
	}
}
