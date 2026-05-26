package diag

import (
	"log/slog"
	"sync"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// ReconnectBuffer holds non-fatal DiagEvents while the WebSocket transport is
// disconnected. On reconnect the service's OnReconnect callback drains the
// buffer through the thingclient. Per spec §7.4 the buffer is bounded by both
// length (default 100) and age (default 5 min); excess events are dropped
// (oldest-first) and bump a Prometheus / opsmetrics counter named
// `diag.dropped_total` so operators can spot a misbehaving WS link.
//
// Behaviour:
//   - Add(evt) prunes any events older than maxAge before appending.
//   - When the buffer is at capacity the oldest event is evicted and the
//     dropped counter is incremented.
//   - Drain() returns and clears the buffer; the caller pushes each event
//     and is responsible for deciding what to do on partial send failure
//     (today: best-effort, log + drop).
//
// ReconnectBuffer is goroutine-safe.
type ReconnectBuffer struct {
	mu      sync.Mutex
	events  []bufferedEvent
	maxLen  int
	maxAge  time.Duration
	dropped *opsmetrics.CounterPin
	clock   func() time.Time
	log     *slog.Logger
}

// bufferedEvent pairs an event with the wall-clock instant it entered the
// buffer so age-based pruning is deterministic regardless of the OccurredAt
// field on the embedded DiagEvent.
type bufferedEvent struct {
	enteredAt time.Time
	evt       opsmetrics.DiagEvent
}

// ReconnectBufferConfig parameterizes NewReconnectBuffer. MaxLen / MaxAge
// default to the spec §7.4 ceiling (100 events / 5 minutes); Dropped is
// optional — when nil the buffer still runs but no counter is bumped on
// overflow.
type ReconnectBufferConfig struct {
	MaxLen  int
	MaxAge  time.Duration
	Dropped *opsmetrics.CounterPin
	Clock   func() time.Time
	Log     *slog.Logger
}

// NewReconnectBuffer constructs a ReconnectBuffer with the supplied config.
// A zero MaxLen falls back to 100; a zero MaxAge falls back to 5 minutes.
// Clock defaults to time.Now (use a closure in tests to control eviction
// without sleeping).
func NewReconnectBuffer(cfg ReconnectBufferConfig) *ReconnectBuffer {
	if cfg.MaxLen <= 0 {
		cfg.MaxLen = 100
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 5 * time.Minute
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &ReconnectBuffer{
		maxLen:  cfg.MaxLen,
		maxAge:  cfg.MaxAge,
		dropped: cfg.Dropped,
		clock:   cfg.Clock,
		log:     cfg.Log,
	}
}

// Add buffers an event. Stale events (entered earlier than now-maxAge) are
// pruned first; if the buffer is still at capacity the oldest entry is
// dropped and the dropped counter is incremented.
func (r *ReconnectBuffer) Add(evt opsmetrics.DiagEvent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock()
	r.pruneStaleLocked(now)

	if len(r.events) >= r.maxLen {
		// Evict the oldest by entered-at order. events is append-ordered so
		// index 0 is the oldest survivor.
		r.events = r.events[1:]
		if r.dropped != nil {
			r.dropped.Inc()
		}
		if r.log != nil {
			r.log.Debug("reconnect buffer overflow; dropped oldest")
		}
	}
	r.events = append(r.events, bufferedEvent{enteredAt: now, evt: evt})
}

// Drain returns and clears the buffer. The caller is responsible for shipping
// each event via the thingclient. Stale events are pruned first so the
// caller never re-emits an event older than maxAge (avoids resurrecting old
// noise after a long disconnect).
func (r *ReconnectBuffer) Drain() []opsmetrics.DiagEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pruneStaleLocked(r.clock())
	if len(r.events) == 0 {
		return nil
	}
	out := make([]opsmetrics.DiagEvent, len(r.events))
	for i, b := range r.events {
		out[i] = b.evt
	}
	r.events = r.events[:0]
	return out
}

// Pending returns the current buffered count. Used by health and diagnostic
// surfaces.
func (r *ReconnectBuffer) Pending() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneStaleLocked(r.clock())
	return len(r.events)
}

// pruneStaleLocked removes events whose enteredAt is older than maxAge.
// Caller must hold r.mu. Stale events are NOT counted toward dropped — they
// are expected (the WS was down longer than the buffer's retention).
func (r *ReconnectBuffer) pruneStaleLocked(now time.Time) {
	cutoff := now.Add(-r.maxAge)
	idx := 0
	for idx < len(r.events) && r.events[idx].enteredAt.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		r.events = append(r.events[:0], r.events[idx:]...)
	}
}
