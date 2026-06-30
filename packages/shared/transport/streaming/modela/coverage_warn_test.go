package modela

import (
	"context"
	"log/slog"
	"testing"
)

// countingHandler records how many log records at each level were emitted.
type countingHandler struct {
	warns int
	last  map[string]any
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countingHandler) WithGroup(string) slog.Handler            { return h }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warns++
		h.last = map[string]any{}
		r.Attrs(func(a slog.Attr) bool { h.last[a.Key] = a.Value.Any(); return true })
	}
	return nil
}

func TestWarnStreamingCoverageGap(t *testing.T) {
	h := &countingHandler{}
	logger := slog.New(h)

	// Below the window: no warning (the normal case).
	WarnStreamingCoverageGap(logger, 100, DefaultTailWindowBytes)
	if h.warns != 0 {
		t.Fatalf("below-window must not warn, got %d", h.warns)
	}

	// At/above the window, first sight: exactly one warning carrying the sizes.
	const big = 987654 // unique, unlikely to collide with other tests' keys
	WarnStreamingCoverageGap(logger, big, DefaultTailWindowBytes)
	if h.warns != 1 {
		t.Fatalf("at/above-window first sight must warn once, got %d", h.warns)
	}
	if h.last["maxPatternBytes"] != int64(big) {
		t.Errorf("warn maxPatternBytes = %v, want %d", h.last["maxPatternBytes"], big)
	}
	if h.last["tailWindowBytes"] != int64(DefaultTailWindowBytes) {
		t.Errorf("warn tailWindowBytes = %v, want %d", h.last["tailWindowBytes"], DefaultTailWindowBytes)
	}

	// Same bound again: deduped, no second warning.
	WarnStreamingCoverageGap(logger, big, DefaultTailWindowBytes)
	if h.warns != 1 {
		t.Fatalf("repeated same-bound call must be deduped, got %d warns", h.warns)
	}

	// Nil logger must not panic.
	WarnStreamingCoverageGap(nil, big+1, DefaultTailWindowBytes)
}
