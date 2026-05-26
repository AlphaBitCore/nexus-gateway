package rollup

import (
	"math"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// classifyShort is the pure classification rule. Tests pin every
// boundary in the matrix so future tweaks must be intentional (and visible
// in the diff).
func TestClassifyShort_Boundaries(t *testing.T) {
	thr := credstate.DefaultThresholds // healthy >=95, degraded >=50, min 5
	cases := []struct {
		name           string
		w              windowCounts
		priorStatus    string
		hasLongSamples bool
		wantStatus     string
		wantRate       float64
	}{
		// zero samples, no prior, no long: Unknown (first-time credential)
		{"zero samples, no prior", windowCounts{}, "", false, credstate.HealthUnknown, 0},
		// zero samples, prior Healthy, long has samples: preserve Healthy
		// (idle / cache-dominated window — don't punish a working credential)
		{"zero samples, prior healthy + long samples", windowCounts{}, credstate.HealthHealthy, true, credstate.HealthHealthy, 0},
		// zero samples, prior Degraded, long has samples: preserve Degraded
		{"zero samples, prior degraded + long samples", windowCounts{}, credstate.HealthDegraded, true, credstate.HealthDegraded, 0},
		// zero samples, prior Healthy but NO long samples: force Unknown
		// (the credential is genuinely silent — no recent evidence at all)
		{"zero samples, prior healthy + no long samples", windowCounts{}, credstate.HealthHealthy, false, credstate.HealthUnknown, 0},
		// zero samples, prior Unknown: stay Unknown
		{"zero samples, prior unknown + long samples", windowCounts{}, credstate.HealthUnknown, true, credstate.HealthUnknown, 0},
		{"below min samples", windowCounts{samples: 4, success: 4}, "", false, credstate.HealthCollecting, 1.0},
		{"exact healthy boundary", windowCounts{samples: 5, success: 5}, "", false, credstate.HealthHealthy, 1.0},
		{"95 percent exactly", windowCounts{samples: 20, success: 19}, "", false, credstate.HealthHealthy, 0.95},
		{"just under healthy", windowCounts{samples: 20, success: 18, upstream5xx: 2}, "", false, credstate.HealthDegraded, 0.9},
		{"50 percent boundary", windowCounts{samples: 10, success: 5, upstream5xx: 5}, "", false, credstate.HealthDegraded, 0.5},
		{"just under degraded", windowCounts{samples: 10, success: 4, upstream5xx: 6}, "", false, credstate.HealthUnavailable, 0.4},
		{"all failures", windowCounts{samples: 10, success: 0, authFail: 10}, "", false, credstate.HealthUnavailable, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, rate, _ := classifyShort(c.w, c.priorStatus, c.hasLongSamples, thr)
			if status != c.wantStatus {
				t.Errorf("status: got %q want %q", status, c.wantStatus)
			}
			if math.Abs(rate-c.wantRate) > 1e-9 {
				t.Errorf("rate: got %v want %v", rate, c.wantRate)
			}
		})
	}
}

func TestDominantErrorOf(t *testing.T) {
	cases := []struct {
		name string
		w    windowCounts
		want string
	}{
		{"no failures", windowCounts{samples: 10, success: 10}, credstate.DominantNone},
		{"all auth fail", windowCounts{samples: 10, success: 0, authFail: 10}, credstate.DominantAuthFail},
		{"mostly 5xx", windowCounts{samples: 10, success: 2, upstream5xx: 7, timeout: 1}, credstate.DominantUpstream5xx},
		{"mostly timeout", windowCounts{samples: 10, success: 3, timeout: 5, upstream5xx: 2}, credstate.DominantTimeout},
		{"mixed below 50pct", windowCounts{samples: 10, success: 0, authFail: 4, upstream5xx: 3, timeout: 3}, credstate.DominantMixed},
		{"client error wins", windowCounts{samples: 10, success: 1, clientError: 6, timeout: 3}, credstate.DominantClientError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dominantErrorOf(c.w); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestClassifyTrend(t *testing.T) {
	pf := func(f float64) *float64 { return &f }
	cases := []struct {
		name  string
		short float64
		long  *float64
		want  string
	}{
		{"no baseline", 0.8, nil, credstate.TrendStable},
		{"matches baseline", 0.9, pf(0.9), credstate.TrendStable},
		{"slightly improved", 0.92, pf(0.9), credstate.TrendStable},
		{"clearly improved", 0.95, pf(0.85), credstate.TrendImproving},
		{"clearly degraded", 0.8, pf(0.9), credstate.TrendDegrading},
		{"barely on degrading edge", 0.85, pf(0.9), credstate.TrendDegrading},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyTrend(c.short, c.long); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// Threshold constants are part of our SDD acceptance criteria — fail the
// build loudly if they change so the docs are updated in lockstep.
func TestThresholdConstants(t *testing.T) {
	if credstate.DefaultThresholds.HealthMinSamples != 5 {
		t.Errorf("DefaultThresholds.HealthMinSamples changed; update docs/developers/architecture/control-plane/credentials-architecture.md")
	}
	if credstate.DefaultThresholds.HealthyThresholdPct != 95 {
		t.Errorf("DefaultThresholds.HealthyThresholdPct changed; update docs")
	}
	if credstate.DefaultThresholds.DegradedThresholdPct != 50 {
		t.Errorf("DefaultThresholds.DegradedThresholdPct changed; update docs")
	}
	if longWindowMultiplier != 12 {
		t.Errorf("longWindowMultiplier changed; update trend documentation")
	}
}
