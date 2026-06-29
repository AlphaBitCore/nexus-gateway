package consumer

import (
	"context"
	"testing"
	"time"
)

// TestPaceDrain_DutyCycleSleepsProportionally verifies the drain pacing yields
// CPU in proportion to the flush cost: a duty cycle of 0.5 sleeps ~= the flush
// duration (work:idle = 1:1), and the sleep is bounded by maxDrainPaceSleep.
func TestPaceDrain_DutyCycleSleepsProportionally(t *testing.T) {
	cases := []struct {
		name     string
		duty     float64
		flushDur time.Duration
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{"disabled_zero", 0, 50 * time.Millisecond, 0, 5 * time.Millisecond},
		{"disabled_one", 1, 50 * time.Millisecond, 0, 5 * time.Millisecond},
		{"disabled_gt_one", 2, 50 * time.Millisecond, 0, 5 * time.Millisecond},
		// duty 0.5 → sleep == flushDur (1/0.5 - 1 = 1).
		{"half_duty", 0.5, 20 * time.Millisecond, 15 * time.Millisecond, 60 * time.Millisecond},
		// A huge flush is capped at maxDrainPaceSleep (250ms), not 9s.
		{"capped", 0.5, 10 * time.Second, 200 * time.Millisecond, 400 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &TrafficEventWriter{cfg: TrafficEventWriterConfig{DrainDutyCycle: tc.duty}}
			start := time.Now()
			w.paceDrain(context.Background(), tc.flushDur)
			elapsed := time.Since(start)
			if elapsed < tc.wantMin {
				t.Fatalf("paceDrain slept %v, want >= %v", elapsed, tc.wantMin)
			}
			if elapsed > tc.wantMax {
				t.Fatalf("paceDrain slept %v, want <= %v", elapsed, tc.wantMax)
			}
		})
	}
}

// TestPressureToDuty_MapsContentionToBackoff verifies the adaptive mapping:
// spare CPU (low overshoot) → full-speed drain, saturation → floored backoff,
// linear in between.
func TestPressureToDuty_MapsContentionToBackoff(t *testing.T) {
	cases := []struct {
		name     string
		pressure float64
		want     float64
	}{
		{"idle_zero", 0, 1.0},
		{"below_low", lowPressure - 0.01, 1.0},
		{"at_low", lowPressure, 1.0},
		{"saturated", highPressure, minAdaptiveDuty},
		{"above_high", highPressure + 5, minAdaptiveDuty},
		// midpoint between low and high → midpoint between 1.0 and minAdaptiveDuty.
		{"midpoint", (lowPressure + highPressure) / 2, 1.0 - 0.5*(1.0-minAdaptiveDuty)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pressureToDuty(tc.pressure)
			if got < tc.want-0.001 || got > tc.want+0.001 {
				t.Fatalf("pressureToDuty(%v) = %v, want %v", tc.pressure, got, tc.want)
			}
		})
	}
}

// TestAdaptivePacer_StartsUnthrottledAndProbes verifies the pacer starts at full
// duty (1.0) and the probe goroutine keeps it in (0,1] without panicking; on an
// unloaded test box the duty should stay near 1.0 (little scheduling overshoot).
func TestAdaptivePacer_StartsUnthrottledAndProbes(t *testing.T) {
	p := newAdaptiveDrainPacer()
	if d := p.duty(); d != 1.0 {
		t.Fatalf("initial duty = %v, want 1.0", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	time.Sleep(120 * time.Millisecond) // let several probe cycles run
	if d := p.duty(); d <= 0 || d > 1.0 {
		t.Fatalf("duty after probing = %v, want in (0,1]", d)
	}
}

// TestDrainDuty_AdaptiveVsFixed verifies drainDuty() reads the live adaptive
// value when a pacer is present, and the fixed config value otherwise.
func TestDrainDuty_AdaptiveVsFixed(t *testing.T) {
	// Adaptive: pacer present, returns its live duty regardless of cfg.
	wa := &TrafficEventWriter{cfg: TrafficEventWriterConfig{DrainDutyCycle: 0}, pacer: newAdaptiveDrainPacer()}
	if d := wa.drainDuty(); d != 1.0 {
		t.Fatalf("adaptive drainDuty = %v, want pacer's 1.0", d)
	}
	// Fixed: no pacer, returns cfg value.
	wf := &TrafficEventWriter{cfg: TrafficEventWriterConfig{DrainDutyCycle: 0.3}}
	if d := wf.drainDuty(); d != 0.3 {
		t.Fatalf("fixed drainDuty = %v, want 0.3", d)
	}
}

// TestDrainDuty_BacklogOverride verifies a dangerous NATS backlog overrides the
// throttle (fixed OR adaptive) to full-speed, while a safe/invalid reading does not.
func TestDrainDuty_BacklogOverride(t *testing.T) {
	// Fixed 0.3 throttle, backlog at/above the override threshold → full-speed.
	wHigh := (&TrafficEventWriter{cfg: TrafficEventWriterConfig{DrainDutyCycle: 0.3}}).
		WithBacklogProbe(func() (float64, bool) { return backlogDutyOverride, true })
	if d := wHigh.drainDuty(); d != 1.0 {
		t.Fatalf("backlog at threshold: drainDuty = %v, want 1.0 override", d)
	}
	// Backlog below threshold → keep the configured throttle.
	wLow := &TrafficEventWriter{
		cfg:          TrafficEventWriterConfig{DrainDutyCycle: 0.3},
		backlogProbe: func() (float64, bool) { return backlogDutyOverride - 0.1, true },
	}
	if d := wLow.drainDuty(); d != 0.3 {
		t.Fatalf("backlog below threshold: drainDuty = %v, want 0.3", d)
	}
	// Invalid reading (ok=false) → no override.
	wInvalid := &TrafficEventWriter{
		cfg:          TrafficEventWriterConfig{DrainDutyCycle: 0.3},
		backlogProbe: func() (float64, bool) { return 0.99, false },
	}
	if d := wInvalid.drainDuty(); d != 0.3 {
		t.Fatalf("invalid backlog reading: drainDuty = %v, want 0.3 (no override)", d)
	}
	// Adaptive mode is overridden too.
	wAdaptive := &TrafficEventWriter{
		cfg:          TrafficEventWriterConfig{DrainDutyCycle: 0},
		pacer:        newAdaptiveDrainPacer(),
		backlogProbe: func() (float64, bool) { return 0.95, true },
	}
	if d := wAdaptive.drainDuty(); d != 1.0 {
		t.Fatalf("adaptive + high backlog: drainDuty = %v, want 1.0 override", d)
	}
}

// TestStartBacklogSampler_CachesLiveFraction verifies Start's sampler primes the
// cached probe from the live query so drainDuty can react.
func TestStartBacklogSampler_CachesLiveFraction(t *testing.T) {
	w := (&TrafficEventWriter{cfg: TrafficEventWriterConfig{DrainDutyCycle: 0.3}}).
		WithBacklogSampler(func(context.Context) (float64, bool) { return 0.8, true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.startBacklogSampler(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := w.backlogProbe(); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	frac, ok := w.backlogProbe()
	if !ok || frac != 0.8 {
		t.Fatalf("cached probe = %v/%v, want 0.8/true", frac, ok)
	}
	if d := w.drainDuty(); d != 1.0 {
		t.Fatalf("high cached backlog should override drainDuty to 1.0, got %v", d)
	}
}

// TestStartBacklogSampler_NoopWhenUnset: no sampler and no probe → no override,
// drainDuty falls through to the configured value.
func TestStartBacklogSampler_NoopWhenUnset(t *testing.T) {
	w := &TrafficEventWriter{cfg: TrafficEventWriterConfig{DrainDutyCycle: 0.3}}
	w.startBacklogSampler(context.Background()) // no-op: backlogSampleFn nil
	if w.backlogProbe != nil {
		t.Fatal("no sampler → backlogProbe should stay nil")
	}
	if d := w.drainDuty(); d != 0.3 {
		t.Fatalf("drainDuty = %v, want 0.3", d)
	}
}

// TestPaceDrain_ContextCancelEndsSleepEarly verifies a cancelled context cuts the
// pacing sleep short so shutdown is not delayed by the duty-cycle idle window.
func TestPaceDrain_ContextCancelEndsSleepEarly(t *testing.T) {
	w := &TrafficEventWriter{cfg: TrafficEventWriterConfig{DrainDutyCycle: 0.01}} // would sleep ~99x flushDur
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	w.paceDrain(ctx, 50*time.Millisecond) // uncapped target ~250ms (the cap), cancel at 10ms
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("paceDrain ignored ctx cancel: slept %v", elapsed)
	}
}
