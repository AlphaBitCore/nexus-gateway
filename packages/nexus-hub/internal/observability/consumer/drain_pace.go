package consumer

import (
	"context"
	"math"
	"sync/atomic"
	"time"
)

// Adaptive drain pacing — the audit drain yields CPU to a co-located AI-gateway's
// core request path ONLY when the box is actually contended, and runs full-speed
// when there is spare CPU. The contention signal is a dependency-free,
// cross-platform timer-overshoot probe: a goroutine sleeps for a fixed interval
// and measures how much longer than that it actually took to be scheduled. On an
// idle box a 20 ms sleep returns in ~20 ms (overshoot ~0); when another process
// (the gateway) is saturating the cores, the OS de-schedules the Hub's threads
// and the same sleep returns late (overshoot rises). That overshoot ratio is a
// direct, real-time measure of "how little CPU is left for me," which is exactly
// the signal the user asked the backoff to react to — not a fixed duty cycle.
//
// The probe drives a duty cycle in (0,1]: 1.0 = drain full-speed (idle box),
// falling toward minAdaptiveDuty as pressure rises (saturated box). paceDrain
// then sleeps after each flush in proportion to that duty, so the drain occupies
// at most `duty` of wall-clock and cedes the rest. The NATS file store (local
// disk, effectively unbounded) absorbs the backlog while the drain idles; audit
// is delay-tolerant and no-loss is preserved by NATS retention, not by racing
// the gateway.

const (
	// probeInterval is how long the pressure probe sleeps each cycle. Short enough
	// to react within a couple hundred ms, long enough that the probe itself costs
	// nothing measurable.
	probeInterval = 20 * time.Millisecond

	// lowPressure / highPressure bound the overshoot ratio mapped to duty. Below
	// lowPressure the box has spare CPU → full-speed drain (duty 1.0). At/above
	// highPressure the box is saturated → duty pinned at minAdaptiveDuty. Overshoot
	// is (actualSleep-probeInterval)/probeInterval, so 0.15 ≈ "sleeps run 15% long"
	// (normal scheduler jitter) and 1.0 ≈ "sleeps take twice as long" (heavy
	// contention).
	lowPressure  = 0.08
	highPressure = 0.6

	// minAdaptiveDuty floors the adaptive duty so the drain never fully stalls even
	// under sustained saturation — it keeps draining at 20% of wall-clock so the
	// NATS backlog still trends down between request bursts.
	minAdaptiveDuty = 0.2

	// pressureEMAAlpha smooths the per-probe overshoot into an exponential moving
	// average so a single jittery sample does not swing the duty cycle.
	pressureEMAAlpha = 0.3
)

// adaptiveDrainPacer holds the current adaptively-computed duty cycle, updated by
// a background pressure probe and read by paceDrain. The duty is stored as
// float64 bits in an atomic so the probe (writer) and the per-queue drain
// goroutines (readers) need no lock.
type adaptiveDrainPacer struct {
	dutyBits atomic.Uint64
}

func newAdaptiveDrainPacer() *adaptiveDrainPacer {
	p := &adaptiveDrainPacer{}
	p.dutyBits.Store(math.Float64bits(1.0)) // start un-throttled until the probe reads pressure
	return p
}

// duty returns the current adaptive duty cycle in (0,1].
func (p *adaptiveDrainPacer) duty() float64 {
	return math.Float64frombits(p.dutyBits.Load())
}

// pressureToDuty maps a smoothed overshoot ratio to a duty cycle: 1.0 at/below
// lowPressure, minAdaptiveDuty at/above highPressure, linear in between.
func pressureToDuty(pressure float64) float64 {
	if pressure <= lowPressure {
		return 1.0
	}
	if pressure >= highPressure {
		return minAdaptiveDuty
	}
	frac := (pressure - lowPressure) / (highPressure - lowPressure)
	return 1.0 - frac*(1.0-minAdaptiveDuty)
}

// run is the background pressure probe. It sleeps probeInterval, measures the
// scheduling overshoot, smooths it, and republishes the derived duty cycle until
// ctx is cancelled.
func (p *adaptiveDrainPacer) run(ctx context.Context) {
	ema := 0.0
	for {
		start := time.Now()
		t := time.NewTimer(probeInterval)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
		overshoot := float64(time.Since(start)-probeInterval) / float64(probeInterval)
		if overshoot < 0 {
			overshoot = 0
		}
		ema = (1-pressureEMAAlpha)*ema + pressureEMAAlpha*overshoot
		p.dutyBits.Store(math.Float64bits(pressureToDuty(ema)))
	}
}
