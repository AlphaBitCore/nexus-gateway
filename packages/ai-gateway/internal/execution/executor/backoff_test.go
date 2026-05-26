package executor

import (
	"testing"
	"time"

	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

func TestComputeBackoff_DoublesUntilMax(t *testing.T) {
	p := configtypes.RetryPolicy{
		BackoffInitial: 250 * time.Millisecond,
		BackoffMax:     5 * time.Second,
		BackoffJitter:  0,
	}
	cases := []struct {
		tryIdx int
		want   time.Duration
	}{
		{1, 250 * time.Millisecond},
		{2, 500 * time.Millisecond},
		{3, 1 * time.Second},
		{4, 2 * time.Second},
		{5, 4 * time.Second},
		{6, 5 * time.Second},
		{7, 5 * time.Second},
		{8, 5 * time.Second},
	}
	for _, tc := range cases {
		if got := computeBackoff(tc.tryIdx, p); got != tc.want {
			t.Errorf("tryIdx=%d: got %v, want %v", tc.tryIdx, got, tc.want)
		}
	}
}

func TestComputeBackoff_JitterStaysInRange(t *testing.T) {
	p := configtypes.RetryPolicy{
		BackoffInitial: 1 * time.Second,
		BackoffMax:     1 * time.Second,
		BackoffJitter:  0.2,
	}
	base := 1 * time.Second
	lower := time.Duration(float64(base) * 0.8)
	upper := time.Duration(float64(base) * 1.2)
	for i := range 1000 {
		got := computeBackoff(1, p)
		if got < lower-time.Microsecond || got > upper+time.Microsecond {
			t.Fatalf("iteration %d: %v out of [%v, %v]", i, got, lower, upper)
		}
	}
}

func TestComputeBackoff_NeverNegative(t *testing.T) {
	p := configtypes.RetryPolicy{
		BackoffInitial: 1 * time.Millisecond,
		BackoffMax:     1 * time.Millisecond,
		BackoffJitter:  0.99,
	}
	for i := range 1000 {
		if got := computeBackoff(1, p); got < 0 {
			t.Fatalf("iteration %d: negative %v", i, got)
		}
	}
}

func TestComputeBackoff_ClampInvalidTryIdx(t *testing.T) {
	p := configtypes.RetryPolicy{
		BackoffInitial: 100 * time.Millisecond,
		BackoffMax:     1 * time.Second,
	}
	if got := computeBackoff(0, p); got != 100*time.Millisecond {
		t.Errorf("tryIdx=0 should clamp to 1: got %v", got)
	}
	if got := computeBackoff(-3, p); got != 100*time.Millisecond {
		t.Errorf("tryIdx=-3 should clamp to 1: got %v", got)
	}
}
