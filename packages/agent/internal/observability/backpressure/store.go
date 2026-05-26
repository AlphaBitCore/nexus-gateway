// Package backpressure exposes a hot-path-safe throttle flag so the
// agent's NE bridge can short-circuit incoming flows to passthrough
// when the local audit queue depth crosses a configured high-water
// mark. Hysteresis prevents flapping: once throttled, the flag stays
// on until depth drops below the low-water mark.
//
// The flag is a single atomic.Bool — handleNewFlow checks it on the
// hot path with no SQL, no mutex, sub-microsecond. A background
// goroutine in main.go polls the actual queue count via Queue.UnsyncedCount()
// every PollInterval and calls Update, so the cost of measuring queue
// depth (one sqlite COUNT) is borne off-hot-path.
package backpressure

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Config holds the thresholds + poll cadence the Store reads on construction.
type Config struct {
	// HighWatermark — Update enters throttle mode when current >= HighWatermark.
	// Defaults to 500 if zero. Tuned from prod observation that the agent
	// stays comfortably caught up under typical workloads at <100 events
	// in flight; 500 is a clear "something's wrong with upload" signal.
	HighWatermark int
	// LowWatermark — Update exits throttle mode when current <= LowWatermark.
	// Must be < HighWatermark; defaults to 200 if zero. Hysteresis gap of
	// 300 events ≈ a few seconds of intercept traffic at typical rates,
	// so the flag doesn't flap on every single Hub upload batch round.
	LowWatermark int
	// PollInterval — how often the background goroutine refreshes the
	// queue depth. Defaults to 2s if zero. Lower values ≈ tighter
	// reaction time at the cost of more sqlite COUNT() calls; higher
	// values ≈ more flows pass through stale-throttled decisions
	// during a sudden spike.
	PollInterval time.Duration
	// Logger — emits INFO on every transition (enter/exit). Optional;
	// nil → slog.Default().
	Logger *slog.Logger
}

// DefaultConfig returns the recommended thresholds.
func DefaultConfig() Config {
	return Config{
		HighWatermark: 500,
		LowWatermark:  200,
		PollInterval:  2 * time.Second,
	}
}

// Store carries the live throttle flag + the configured thresholds.
// Constructed once at agent boot; Update is called from the polling
// goroutine; IsThrottled is called from the bridge / NE flow handlers
// on every new flow.
type Store struct {
	cfg       Config
	throttled atomic.Bool
	logger    *slog.Logger
}

// NewStore returns a Store seeded with the supplied config. Missing
// config fields fall back to DefaultConfig values; LowWatermark >=
// HighWatermark is rejected (returns DefaultConfig instead) since
// a missing-hysteresis Store would flap on every Update call.
func NewStore(cfg Config) *Store {
	if cfg.HighWatermark <= 0 {
		cfg.HighWatermark = 500
	}
	if cfg.LowWatermark <= 0 {
		cfg.LowWatermark = 200
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.LowWatermark >= cfg.HighWatermark {
		// Disallowed combination — fall back to defaults so the agent
		// boots in a known-good state instead of flapping.
		cfg.HighWatermark = 500
		cfg.LowWatermark = 200
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{cfg: cfg, logger: logger}
}

// IsThrottled returns true when the agent is currently shedding load
// because the audit queue depth is above LowWatermark and was, at some
// recent measurement, above HighWatermark. Lock-free; cheap enough to
// call on every NE flow.
func (s *Store) IsThrottled() bool {
	if s == nil {
		return false
	}
	return s.throttled.Load()
}

// Update is called by the background poller with the latest measured
// queue depth. Applies hysteresis: enters throttle only when crossing
// HighWatermark from below, exits only when crossing LowWatermark from
// above. A single Update call may not change state.
func (s *Store) Update(currentDepth int) {
	if s == nil {
		return
	}
	was := s.throttled.Load()
	switch {
	case !was && currentDepth >= s.cfg.HighWatermark:
		// Entering throttle.
		s.throttled.Store(true)
		s.logger.Warn("backpressure: entering throttle",
			"queue_depth", currentDepth,
			"high_watermark", s.cfg.HighWatermark,
			"low_watermark", s.cfg.LowWatermark,
		)
	case was && currentDepth <= s.cfg.LowWatermark:
		// Exiting throttle.
		s.throttled.Store(false)
		s.logger.Info("backpressure: exiting throttle",
			"queue_depth", currentDepth,
			"low_watermark", s.cfg.LowWatermark,
		)
	}
}

// HighWatermark returns the configured enter-throttle threshold. Used
// by status reporters / Diagnostics UI for the badge text.
func (s *Store) HighWatermark() int {
	if s == nil {
		return 0
	}
	return s.cfg.HighWatermark
}

// LowWatermark returns the configured exit-throttle threshold.
func (s *Store) LowWatermark() int {
	if s == nil {
		return 0
	}
	return s.cfg.LowWatermark
}

// DepthSource is the function the background poller calls to measure
// the current queue depth. Concrete implementation is
// `func() int { return queue.UnsyncedCount() }` in main.go; the
// indirection keeps backpressure free of an audit-package import.
type DepthSource func() int

// Poll runs Update at PollInterval until ctx is done. Caller is
// responsible for `go store.Poll(ctx, queue.UnsyncedCount)` once at
// agent boot. Safe to start multiple polls; the atomic.Bool simply
// converges.
func (s *Store) Poll(ctx context.Context, source DepthSource) {
	if s == nil || source == nil {
		return
	}
	tick := time.NewTicker(s.cfg.PollInterval)
	defer tick.Stop()
	// Prime once so the first throttle-onset doesn't wait a full
	// PollInterval.
	s.Update(source())
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.Update(source())
		}
	}
}
