package alerteval

import (
	"sort"
	"sync"
	"time"
)

// SampleWindow is a fixed-cap ring buffer of (timestamp, value) pairs used
// by aggregators that need order-statistic readouts (median / p95 / pXX)
// of a sliding window. Distinct from `Window` which only sums per-second
// buckets — a per-second bucket loses the individual sample shape that
// percentile math needs.
//
// On overflow the oldest sample is overwritten — common reservoir-style
// behaviour. cap = 1000 is enough for ~3 req/s sustained over a 5-minute
// window, which is the upper end of what one provider/VK typically
// drives. Higher-volume targets get an approximate p95 from the most
// recent N samples — accurate enough for trending vs an absolute SLO.
type SampleWindow struct {
	mu      sync.Mutex
	samples []timestamped
	head    int
	full    bool
}

type timestamped struct {
	t time.Time
	v float64
}

// NewSampleWindow creates a SampleWindow with the given fixed capacity.
// capSamples must be >= 1; passing < 1 panics (caller bug).
func NewSampleWindow(capSamples int) *SampleWindow {
	if capSamples < 1 {
		panic("alerteval: NewSampleWindow capSamples must be >= 1")
	}
	return &SampleWindow{samples: make([]timestamped, 0, capSamples)}
}

// Add records a sample. When the buffer is full the oldest entry is
// overwritten in place.
func (w *SampleWindow) Add(at time.Time, value float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.full && len(w.samples) < cap(w.samples) {
		w.samples = append(w.samples, timestamped{t: at, v: value})
		if len(w.samples) == cap(w.samples) {
			w.full = true
		}
		return
	}
	w.samples[w.head] = timestamped{t: at, v: value}
	w.head = (w.head + 1) % cap(w.samples)
}

// Percentile returns the p-th percentile (0..100) of the samples whose
// timestamp falls within `lookback` ending at `now`. Returns (0, 0)
// when no samples are in window. The returned `count` lets the caller
// gate on minSamples without a separate query.
func (w *SampleWindow) Percentile(lookback time.Duration, now time.Time, p float64) (val float64, count int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.samples) == 0 {
		return 0, 0
	}
	cutoff := now.Add(-lookback)
	in := make([]float64, 0, len(w.samples))
	for _, s := range w.samples {
		if s.t.After(cutoff) || s.t.Equal(cutoff) {
			in = append(in, s.v)
		}
	}
	if len(in) == 0 {
		return 0, 0
	}
	sort.Float64s(in)
	// Nearest-rank percentile, clamped to [0, len-1].
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	idx := int((p / 100.0) * float64(len(in)-1))
	return in[idx], len(in)
}
