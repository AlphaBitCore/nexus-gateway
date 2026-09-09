package pipeline

import (
	"log/slog"
	"sync"
	"time"
)

// A hook that is failing on most of the traffic it sees is not the same event as
// a hook that failed once, and the per-request fail-posture cannot tell them
// apart: it answers each execution on its own, correctly, forever, while the
// rule the operator configured has in practice stopped running.
//
// This is a DETECTOR, not a circuit breaker, and the distinction is the whole
// design. A breaker's action is to stop calling the failing dependency; here
// that means either refusing all traffic (a fail-closed hook breaking the
// service on its own) or silently skipping the scan (a fail-open hook doing
// precisely the thing this exists to catch). Both were rejected. So it changes
// no request-level behaviour at all: it produces EVIDENCE — a gauge, a log line
// on each state flip, and a per-request audit tag — and leaves the decision to a
// person.
//
// The per-request tag is the part Prometheus cannot supply. hook_error_total and
// hook_timeout_total already exist and an alert can divide them, but no metric
// answers "which requests were served while the PII detector was degraded"; that
// is a fact about a row, and it belongs on the row, next to hook-unbuildable
// which makes the same kind of claim.
const (
	// degradeWindow is the owner's number. Short on purpose: this is meant to
	// notice a hook that broke moments ago, and it is deliberately finer than
	// any scrape interval, which is the other reason it cannot be a Prometheus
	// rule.
	degradeWindow = 3 * time.Second
	// Six buckets, so the window slides in 500ms steps rather than resetting
	// whole. A single bucket would make the rate jump to zero every 3 seconds
	// and the state flap with it.
	degradeBuckets   = 6
	degradeBucketDur = degradeWindow / degradeBuckets

	// degradeMinSamples keeps a cold start from tripping. The first request
	// after a deploy failing means a rate of 100% over one sample, which says
	// nothing. At any real request rate 3 seconds holds far more than 20; this
	// is a floor against the quiet periods, not a tuning knob.
	degradeMinSamples = 20

	// Half is the natural line between "intermittent" and "not working". Below
	// it, per-execution fail posture is the right handling and this should stay
	// quiet; above it the hook has effectively stopped running.
	degradeTripRate = 0.50
	// Clearing at a LOWER rate is hysteresis. One threshold in both directions
	// oscillates at the boundary and turns the alert into noise.
	degradeClearRate = 0.25
)

// hookWindow is one implementation's sliding failure window.
type hookWindow struct {
	mu      sync.Mutex
	buckets [degradeBuckets]struct {
		epoch       int64
		total, fail int
	}
	degraded bool
	// enteredEpoch is when the current state began. Clearing requires a full
	// window in the new state, so a single quiet bucket cannot clear a hook that
	// is still broken.
	enteredEpoch int64
}

// record adds one execution and reports the state plus whether it just changed.
func (w *hookWindow) record(now time.Time, failed bool) (degraded, flipped bool) {
	epoch := now.UnixNano() / int64(degradeBucketDur)

	w.mu.Lock()
	defer w.mu.Unlock()

	b := &w.buckets[epoch%degradeBuckets]
	if b.epoch != epoch {
		// The slot belongs to an older revolution of the ring; it is this
		// epoch's bucket now and starts empty.
		b.epoch, b.total, b.fail = epoch, 0, 0
	}
	b.total++
	if failed {
		b.fail++
	}

	total, fail := 0, 0
	oldest := epoch - degradeBuckets + 1
	for i := range w.buckets {
		if w.buckets[i].epoch >= oldest && w.buckets[i].epoch <= epoch {
			total += w.buckets[i].total
			fail += w.buckets[i].fail
		}
	}
	if total < degradeMinSamples {
		return w.degraded, false
	}
	rate := float64(fail) / float64(total)

	switch {
	case !w.degraded && rate >= degradeTripRate:
		w.degraded, w.enteredEpoch = true, epoch
		return true, true
	case w.degraded && rate < degradeClearRate && epoch-w.enteredEpoch >= degradeBuckets:
		w.degraded, w.enteredEpoch = false, epoch
		return false, true
	}
	return w.degraded, false
}

func (w *hookWindow) state() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.degraded
}

// hookHealth tracks every implementation's window. It lives on the resolver
// rather than on a Pipeline because pipelines are built per request and a
// per-request window would never accumulate a sample.
type hookHealth struct {
	mu      sync.RWMutex
	windows map[string]*hookWindow
	// now is injectable so the tests state a schedule instead of sleeping
	// through one; a real clock here makes every threshold assertion a race
	// against the machine's load.
	now func() time.Time
}

func newHookHealth() *hookHealth {
	return &hookHealth{windows: map[string]*hookWindow{}, now: time.Now}
}

func (h *hookHealth) windowFor(impl string) *hookWindow {
	h.mu.RLock()
	w := h.windows[impl]
	h.mu.RUnlock()
	if w != nil {
		return w
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if w = h.windows[impl]; w == nil {
		w = &hookWindow{}
		h.windows[impl] = w
	}
	return w
}

// Record notes one execution of impl.
//
// failed means the hook did not produce a verdict — an error, a timeout, a
// panic. A REJECT or a BLOCK is the hook WORKING and must never count here, or
// a policy doing its job would read as a policy that is broken.
func (h *hookHealth) Record(impl string, failed bool, logger *slog.Logger) {
	if h == nil || impl == "" {
		return
	}
	degraded, flipped := h.windowFor(impl).record(h.now(), failed)
	if !flipped {
		return
	}
	// Log the FLIP, never the execution. At a thousand requests a second a
	// per-request line about a broken hook is itself an outage.
	state := "recovered"
	if degraded {
		state = "degraded"
	}
	if logger != nil {
		logger.Warn("compliance hook health changed",
			"hook", impl,
			"state", state,
			"windowSeconds", degradeWindow.Seconds(),
			"tripRate", degradeTripRate,
			"clearRate", degradeClearRate,
		)
	}
	v := 0.0
	if degraded {
		v = 1
	}
	HookDegraded.WithLabelValues(impl).Set(v)
}

// Degraded reports whether impl is currently in the degraded state.
func (h *hookHealth) Degraded(impl string) bool {
	if h == nil || impl == "" {
		return false
	}
	h.mu.RLock()
	w := h.windows[impl]
	h.mu.RUnlock()
	return w != nil && w.state()
}

// degradedTagPrefix marks a request served while a hook was systematically
// failing. Same channel and same shape as hook-unbuildable, because they answer
// the same operator question about different causes.
const degradedTagPrefix = "hook-degraded:"
