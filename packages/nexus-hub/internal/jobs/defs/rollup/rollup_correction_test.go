package rollup

import (
	"testing"
	"time"
)

func TestRollupCorrection_Identity(t *testing.T) {
	r5m := NewRollup5m(nil, 0, testLogger(), false)
	m1h := NewRollupMerge1h(nil, 0, testLogger())
	m1d := NewRollupMerge1d(nil, 0, testLogger())
	m1mo := NewRollupMerge1mo(nil, 0, testLogger())

	j := NewRollupCorrection(r5m, m1h, m1d, m1mo, 7, 24*time.Hour, testLogger())
	if j.ID() != "rollup-correction" {
		t.Errorf("ID = %q, want rollup-correction", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h", j.Interval())
	}
	if j.lookbackDays != 7 {
		t.Errorf("lookbackDays = %d, want 7", j.lookbackDays)
	}
}

func TestRollupCorrection_IntervalDefault(t *testing.T) {
	j := NewRollupCorrection(nil, nil, nil, nil, 0, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h default", j.Interval())
	}
	// lookbackDays <= 0 falls back to the package default.
	if j.lookbackDays != correctionLookbackDays {
		t.Errorf("lookbackDays = %d, want default %d", j.lookbackDays, correctionLookbackDays)
	}
}
