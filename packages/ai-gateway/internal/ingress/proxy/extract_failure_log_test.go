package proxy

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func resetExtractFailCounter() {
	extractFailLogCounts.Range(func(k, _ any) bool {
		extractFailLogCounts.Delete(k)
		return true
	})
}

// The defect this guards is not "no log line" — it is a line at a level nobody
// has on. The counter nexus_traffic_extract_total{outcome=error} would move and
// nothing on the box could say which adapter, which path, or why.
func TestLogExtractFailure_IsVisibleAtWarnAndNamesTheConsequence(t *testing.T) {
	resetExtractFailCounter()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	logExtractFailure(logger, "response", "openai-compat", "/v1/chat/completions", 4096,
		errors.New("unexpected end of JSON input"))

	out := buf.String()
	if out == "" {
		t.Fatal("nothing logged at INFO level — the finding was that this sat at Debug, which is off in practice")
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("want WARN (an operator reads it) and not ERROR (a process failure), got:\n%s", out)
	}
	for _, want := range []string{"openai-compat", "/v1/chat/completions", "unexpected end of JSON input", "body_bytes=4096", "direction=response"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q — a diagnosis without it cannot be acted on:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "consequence") || !strings.Contains(out, "nothing was scanned") {
		t.Errorf("the compliance consequence must be stated, not left to be inferred:\n%s", out)
	}
}

// A malformed provider response repeats per request. Unthrottled, the line that
// exists to be readable would be the reason the log is unreadable.
func TestLogExtractFailure_ThrottlesButKeepsTheRunningTotal(t *testing.T) {
	resetExtractFailCounter()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	const n = extractFailLogEvery * 3
	for range n {
		logExtractFailure(logger, "request", "a", "/p", 1, errors.New("boom"))
	}

	got := strings.Count(buf.String(), "content extract failed")
	if got != 3 {
		t.Errorf("logged %d times for %d failures; want 3 (one per %d)", got, n, extractFailLogEvery)
	}
	// The rate has to be recoverable from the line itself, or a throttled log
	// understates the problem by exactly the throttle factor.
	if !strings.Contains(buf.String(), "occurrences_for_this_adapter=101") {
		t.Errorf("the running total must appear so the rate is visible:\n%s", buf.String())
	}
}

// A nil logger must not panic: the extract path runs per request and is reached
// from callers that may not have bound one.
func TestLogExtractFailure_NilLoggerIsSafe(t *testing.T) {
	resetExtractFailCounter()
	logExtractFailure(nil, "request", "a", "/p", 1, errors.New("boom"))
}

// The throttle is per (direction, adapter) so a noisy adapter cannot absorb the
// whole budget and mask a rare failure elsewhere — and the rare one is precisely
// the failure an operator has no other way to notice.
func TestLogExtractFailure_OneNoisyAdapterDoesNotMaskAnother(t *testing.T) {
	resetExtractFailCounter()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A flood on one adapter...
	for range extractFailLogEvery * 5 {
		logExtractFailure(logger, "response", "noisy-adapter", "/p", 1, errors.New("flood"))
	}
	// ...must not consume the budget of a single failure on another.
	logExtractFailure(logger, "response", "quiet-adapter", "/q", 1, errors.New("rare"))

	out := buf.String()
	if !strings.Contains(out, "quiet-adapter") {
		t.Errorf("the single failure on the quiet adapter was masked by the noisy one:\n%s", out)
	}
	// And the two directions of the same adapter are separate keys, since a
	// request-side and a response-side failure are different problems.
	buf.Reset()
	logExtractFailure(logger, "request", "noisy-adapter", "/p", 1, errors.New("other direction"))
	if !strings.Contains(buf.String(), "direction=request") {
		t.Errorf("the request-side failure was masked by the response-side flood:\n%s", buf.String())
	}
}
