package credstate

import (
	"strings"
	"testing"
)

func TestKeyHelpers(t *testing.T) {
	cases := []struct {
		name    string
		got     string
		wantHas string
	}{
		{"stats", StatsKey("abc"), "cred:stats:abc"},
		{"circuit", CircuitKey("abc"), "cred:circuit:abc"},
		{"in_flight", InFlightSet("hub-1"), "cred:circuit:in_flight:hub-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.wantHas {
				t.Errorf("got %q want %q", c.got, c.wantHas)
			}
		})
	}
}

func TestThresholds_Merge(t *testing.T) {
	base := DefaultThresholds
	override := Thresholds{
		HealthyThresholdPct:  90, // override
		HealthMinSamples:     10, // override
		DegradedThresholdPct: 0,  // zero — keep base
	}
	got := base.Merge(override)

	if got.HealthyThresholdPct != 90 {
		t.Errorf("HealthyThresholdPct: got %d want 90", got.HealthyThresholdPct)
	}
	if got.HealthMinSamples != 10 {
		t.Errorf("HealthMinSamples: got %d want 10", got.HealthMinSamples)
	}
	if got.DegradedThresholdPct != base.DegradedThresholdPct {
		t.Errorf("DegradedThresholdPct: got %d want %d (unchanged)", got.DegradedThresholdPct, base.DegradedThresholdPct)
	}
	if got.AuthFailThreshold != base.AuthFailThreshold {
		t.Errorf("AuthFailThreshold: untouched fields must come from base")
	}
}

func TestThresholds_MergeEveryFieldOverrides(t *testing.T) {
	// Pin: each Threshold field has its own override branch in Merge.
	// A refactor that combines branches or drops one would silently
	// stop honouring that override. Test every field individually.
	base := Thresholds{
		AuthFailThreshold:              1,
		RateLimitCooldownSeconds:       2,
		HealthyThresholdPct:            10,
		DegradedThresholdPct:           5,
		HealthMinSamples:               3,
		HealthWindowSeconds:            4,
		HealthSustainedDegradedSeconds: 6,
	}
	cases := []struct {
		name     string
		override Thresholds
		want     Thresholds
	}{
		{"AuthFailThreshold", Thresholds{AuthFailThreshold: 99},
			Thresholds{99, 2, 10, 5, 3, 4, 6}},
		{"RateLimitCooldownSeconds", Thresholds{RateLimitCooldownSeconds: 99},
			Thresholds{1, 99, 10, 5, 3, 4, 6}},
		{"HealthyThresholdPct", Thresholds{HealthyThresholdPct: 99},
			Thresholds{1, 2, 99, 5, 3, 4, 6}},
		{"DegradedThresholdPct", Thresholds{DegradedThresholdPct: 99},
			Thresholds{1, 2, 10, 99, 3, 4, 6}},
		{"HealthMinSamples", Thresholds{HealthMinSamples: 99},
			Thresholds{1, 2, 10, 5, 99, 4, 6}},
		{"HealthWindowSeconds", Thresholds{HealthWindowSeconds: 99},
			Thresholds{1, 2, 10, 5, 3, 99, 6}},
		{"HealthSustainedDegradedSeconds", Thresholds{HealthSustainedDegradedSeconds: 99},
			Thresholds{1, 2, 10, 5, 3, 4, 99}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := base.Merge(c.override)
			if got != c.want {
				t.Errorf("Merge override %s: got %+v, want %+v", c.name, got, c.want)
			}
		})
	}
}

func TestThresholds_MergeZeroFieldsKeepBase(t *testing.T) {
	// Zero in override means "do not override" — pins this contract
	// since the implementation gates on `> 0` not `!= 0`. A future
	// refactor to `!= 0` would silently break this (negative override
	// would now apply).
	base := Thresholds{
		AuthFailThreshold:              5,
		RateLimitCooldownSeconds:       60,
		HealthyThresholdPct:            95,
		DegradedThresholdPct:           50,
		HealthMinSamples:               5,
		HealthWindowSeconds:            300,
		HealthSustainedDegradedSeconds: 900,
	}
	got := base.Merge(Thresholds{})
	if got != base {
		t.Errorf("empty override mutated base: got %+v, want %+v", got, base)
	}
}

func TestThresholds_Validate_EveryBranchRejected(t *testing.T) {
	// Pin each numeric-field zero rejection — without per-field coverage,
	// a refactor that combines branches or drops one would silently
	// admit invalid Thresholds and the circuit-breaker math would
	// later div-by-zero or wrap.
	valid := Thresholds{
		AuthFailThreshold:              3,
		RateLimitCooldownSeconds:       60,
		HealthyThresholdPct:            95,
		DegradedThresholdPct:           50,
		HealthMinSamples:               5,
		HealthWindowSeconds:            300,
		HealthSustainedDegradedSeconds: 900,
	}
	cases := []struct {
		name   string
		mutate func(*Thresholds)
		errSub string
	}{
		{"RateLimitCooldownSeconds zero",
			func(t *Thresholds) { t.RateLimitCooldownSeconds = 0 },
			"rateLimitCooldownSeconds"},
		{"HealthyThresholdPct zero",
			func(t *Thresholds) { t.HealthyThresholdPct = 0; t.DegradedThresholdPct = 0 },
			"healthyThresholdPct"},
		{"DegradedThresholdPct zero",
			func(t *Thresholds) { t.DegradedThresholdPct = 0 },
			"degradedThresholdPct"},
		{"HealthWindowSeconds zero",
			func(t *Thresholds) { t.HealthWindowSeconds = 0 },
			"healthWindowSeconds"},
		{"HealthSustainedDegradedSeconds zero",
			func(t *Thresholds) { t.HealthSustainedDegradedSeconds = 0 },
			"healthSustainedDegradedSeconds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			th := valid
			c.mutate(&th)
			err := th.Validate()
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("%s: error wrong field: %v", c.name, err)
			}
		})
	}
}

func TestThresholds_Validate(t *testing.T) {
	cases := []struct {
		name   string
		t      Thresholds
		errSub string // empty means no error expected
	}{
		{"defaults ok", DefaultThresholds, ""},
		{"healthy <= degraded", Thresholds{
			AuthFailThreshold: 3, RateLimitCooldownSeconds: 60,
			HealthyThresholdPct: 50, DegradedThresholdPct: 50,
			HealthMinSamples: 5, HealthWindowSeconds: 300, HealthSustainedDegradedSeconds: 900,
		}, "degradedThresholdPct"},
		{"healthy > 100", Thresholds{
			AuthFailThreshold: 3, RateLimitCooldownSeconds: 60,
			HealthyThresholdPct: 120, DegradedThresholdPct: 50,
			HealthMinSamples: 5, HealthWindowSeconds: 300, HealthSustainedDegradedSeconds: 900,
		}, "healthyThresholdPct"},
		{"min samples zero", Thresholds{
			AuthFailThreshold: 3, RateLimitCooldownSeconds: 60,
			HealthyThresholdPct: 95, DegradedThresholdPct: 50,
			HealthMinSamples: 0, HealthWindowSeconds: 300, HealthSustainedDegradedSeconds: 900,
		}, "healthMinSamples"},
		{"auth fail zero", Thresholds{
			AuthFailThreshold: 0, RateLimitCooldownSeconds: 60,
			HealthyThresholdPct: 95, DegradedThresholdPct: 50,
			HealthMinSamples: 5, HealthWindowSeconds: 300, HealthSustainedDegradedSeconds: 900,
		}, "authFailThreshold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.t.Validate()
			if c.errSub == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.errSub)
			}
			if !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("expected error containing %q, got %v", c.errSub, err)
			}
		})
	}
}
