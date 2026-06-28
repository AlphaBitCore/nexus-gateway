// traffic_drain.go — the TrafficEventWriter's CPU-yield drain pacing: the
// per-queue worker count (drainWorkersPerQueue), the post-flush throttle sleep
// (paceDrain), and the live duty-cycle decision incl. the backlog override
// (drainDuty). Split from traffic.go, which owns the consume loop these helpers
// pace.
package consumer

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"time"
)

// drainWorkersPerQueue is the number of parallel drain workers per traffic queue.
// They share one BatchAccumulator so the drain pipelines NATS fetch against the
// synchronous PG flush instead of stalling a single fetch loop on every batch. The
// drain is PG-I/O bound (not CPU bound) and the box has spare cores, so the default
// scales with CPU; NEXUS_HUB_DRAIN_WORKERS overrides. Keep it at or below the pgx
// pool size — each in-flight flush takes one connection.
func drainWorkersPerQueue() int {
	if v := os.Getenv("NEXUS_HUB_DRAIN_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	// NumCPU/4 per queue keeps the total (× len(TrafficQueues)) comfortably under
	// the default pgx pool (20), leaving connections for the hub's other PG users.
	// A handful of workers per queue is enough to keep the disk busy while fetch
	// overlaps flush; raise NEXUS_HUB_DRAIN_WORKERS (and the pool) to push further.
	n := runtime.NumCPU() / 4
	if n < 2 {
		n = 2
	}
	if n > 6 {
		n = 6
	}
	return n
}

// paceDrain sleeps after a flush so the drain occupies at most its current duty
// cycle of wall-clock time, ceding the rest to a co-located gateway's core path.
// The duty is the adaptive CPU-pressure-driven value when in adaptive mode, else
// the fixed configured value. A duty d in (0,1) sleeps flushDur*(1/d - 1) (so
// work:idle = d:(1-d)), capped at maxDrainPaceSleep; d >= 1 disables throttling.
// The sleep is ctx-cancellable so shutdown is not delayed.
func (w *TrafficEventWriter) paceDrain(ctx context.Context, flushDur time.Duration) {
	d := w.drainDuty()
	if d <= 0 || d >= 1 || flushDur <= 0 {
		return
	}
	sleep := time.Duration(float64(flushDur) * (1/d - 1))
	if sleep > maxDrainPaceSleep {
		sleep = maxDrainPaceSleep
	}
	if sleep <= 0 {
		return
	}
	t := time.NewTimer(sleep)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// drainDuty returns the duty cycle paceDrain should enforce right now. A dangerous
// NATS backlog (>= backlogDutyOverride of MaxBytes) overrides everything to
// full-speed (1.0), so the drain empties the backlog instead of yielding CPU
// exactly when the stream is approaching MaxBytes/DiscardNew. Otherwise it returns
// the adaptive probe's live value (adaptive mode) or the fixed configured value
// (which may be a (0,1) throttle or >= 1 = off).
func (w *TrafficEventWriter) drainDuty() float64 {
	if w.backlogProbe != nil {
		if frac, ok := w.backlogProbe(); ok && frac >= backlogDutyOverride {
			return 1.0
		}
	}
	if w.pacer != nil {
		return w.pacer.duty()
	}
	return w.cfg.DrainDutyCycle
}
