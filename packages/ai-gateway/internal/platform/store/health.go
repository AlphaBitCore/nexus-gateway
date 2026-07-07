package store

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	healthWindowDuration = 5 * time.Minute
	maxHealthSamples     = 100

	healthThresholdDegraded    = 0.05
	healthThresholdUnavailable = 0.25

	// healthSampleChanCap bounds the hot-path sample buffer. record() does a
	// non-blocking send; a full channel drops the sample (advisory-only, counted
	// in dropped). Sized well above the per-instance record rate so steady load
	// never drops.
	healthSampleChanCap = 8192

	// healthPublishInterval is how often the single writer republishes the
	// immutable snapshot when new samples have been applied. Batching the publish
	// (rather than rebuilding the snapshot on every sample) keeps the per-response
	// hot path allocation-light: a per-sample whole-map rebuild at high request
	// rate would be pure waste, while the health signal feeds only a 5-min-window,
	// reorder-only router where sub-100ms advisory staleness is immaterial.
	healthPublishInterval = 100 * time.Millisecond
)

// HealthStatus represents the health state of a provider.
type HealthStatus string

const (
	HealthStatusHealthy     HealthStatus = "healthy"
	HealthStatusDegraded    HealthStatus = "degraded"
	HealthStatusUnavailable HealthStatus = "unavailable"
)

// healthSample is one recorded outcome. The timestamp is captured in the hot
// path (record), not in the writer, so a queue backlog cannot skew the 5-min
// window.
type healthSample struct {
	timestamp time.Time
	success   bool
	latencyMs int
}

// healthWindow holds one provider's recent samples. It is used both as the
// writer's private mutable window and, with a freshly COPIED samples slice, as
// an immutable snapshot window (see buildSnapshot).
type healthWindow struct {
	providerName string
	samples      []healthSample
}

// healthSnapshot is an immutable, atomically-published view of every provider's
// window. Once published it is never mutated; the writer builds a brand-new
// snapshot (new map + copied sample slices) on each publish, so concurrent
// GetHealth readers holding an older pointer are always race-free.
type healthSnapshot struct {
	windows map[string]*healthWindow
}

// healthMsg rides the single writer channel. A data message carries a sample
// for a provider; a flush message carries an ack channel the writer closes AFTER
// applying every prior (FIFO) sample and republishing the snapshot. The flush
// marker travels the SAME channel as samples so single-channel FIFO ordering
// guarantees a same-goroutine "record then flush" observes its own write.
type healthMsg struct {
	providerID   string
	providerName string
	sample       healthSample
	isFlush      bool
	ack          chan struct{}
}

// HealthTracker tracks provider health using an in-process sliding window of
// samples. It is used exclusively for per-instance routing decisions (avoiding
// unhealthy providers). Durable health state for the status page is computed
// centrally by the Hub ProviderHealthRollupJob over traffic_event.
//
// Concurrency model: a single background writer owns the mutable windows; the
// per-response hot path (record) only does a non-blocking channel send, so it
// never contends a mutex. GetHealth reads an immutable snapshot published via an
// atomic.Pointer, so routing reads are lock-free. This removes the process-wide
// mutex that every upstream response previously serialized on.
type HealthTracker struct {
	ch       chan healthMsg
	stop     chan struct{}
	stopped  chan struct{}
	snap     atomic.Pointer[healthSnapshot]
	dropped  atomic.Int64
	stopOnce sync.Once
}

// NewHealthTracker creates a health tracker and starts its writer goroutine.
// Callers must call Stop when done (production uses a process-lifetime singleton;
// tests should defer/Cleanup Stop) to avoid leaking the writer.
func NewHealthTracker() *HealthTracker {
	ht := &HealthTracker{
		ch:      make(chan healthMsg, healthSampleChanCap),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	// Pre-publish an empty snapshot so a GetHealth that races ahead of the first
	// record never dereferences a nil snapshot.
	ht.snap.Store(&healthSnapshot{windows: map[string]*healthWindow{}})
	go ht.run(healthPublishInterval)
	return ht
}

// RecordSuccess records a successful request to a provider.
func (ht *HealthTracker) RecordSuccess(providerID, providerName string, latencyMs int) {
	ht.record(providerID, providerName, true, latencyMs)
}

// RecordFailure records a failed request to a provider.
func (ht *HealthTracker) RecordFailure(providerID, providerName string, latencyMs int) {
	ht.record(providerID, providerName, false, latencyMs)
}

func (ht *HealthTracker) record(providerID, providerName string, success bool, latencyMs int) {
	ht.recordAt(providerID, providerName, success, latencyMs, time.Now())
}

// recordAt is the timestamp-explicit form of record. Production always passes
// time.Now() (the timestamp is captured on the hot path so a writer backlog
// cannot skew the 5-min window); it is factored out so tests can inject aged
// samples to exercise the read-time window prune.
func (ht *HealthTracker) recordAt(providerID, providerName string, success bool, latencyMs int, ts time.Time) {
	msg := healthMsg{
		providerID:   providerID,
		providerName: providerName,
		sample: healthSample{
			timestamp: ts,
			success:   success,
			latencyMs: latencyMs,
		},
	}
	select {
	case ht.ch <- msg:
	default:
		// Channel full: drop the sample rather than block the request path. The
		// health signal is advisory (the router only reorders, never drops a
		// target), so a dropped sample at most yields a slightly stale ordering.
		ht.dropped.Add(1)
	}
}

// Stop shuts the writer down. Idempotent and safe to call concurrently with
// record: it closes only ht.stop (never ht.ch — a send on a closed channel from
// an in-flight record would panic), then waits for the writer to exit.
func (ht *HealthTracker) Stop() {
	ht.stopOnce.Do(func() {
		close(ht.stop)
		<-ht.stopped
	})
}

// run is the single writer. It owns the mutable windows map exclusively, applies
// each sample on dequeue, and republishes an immutable snapshot on a tick (when
// dirty) or immediately on a flush marker.
func (ht *HealthTracker) run(interval time.Duration) {
	defer close(ht.stopped)

	windows := make(map[string]*healthWindow)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	dirty := false
	publish := func() {
		ht.snap.Store(buildSnapshot(windows))
		dirty = false
	}

	for {
		select {
		case msg := <-ht.ch:
			if msg.isFlush {
				// Every sample enqueued before this marker (FIFO) has been applied
				// above; publish unconditionally so the flush caller observes them,
				// then release the caller.
				publish()
				close(msg.ack)
				continue
			}
			applySample(windows, msg)
			dirty = true
		case <-ticker.C:
			if dirty {
				publish()
			}
		case <-ht.stop:
			return
		}
	}
}

// applySample appends to the provider's private window and caps at the most
// recent maxHealthSamples. Mutates only the writer-private windows map.
func applySample(windows map[string]*healthWindow, msg healthMsg) {
	w := windows[msg.providerID]
	if w == nil {
		w = &healthWindow{providerName: msg.providerName}
		windows[msg.providerID] = w
	}
	w.samples = append(w.samples, msg.sample)
	if len(w.samples) > maxHealthSamples {
		w.samples = w.samples[len(w.samples)-maxHealthSamples:]
	}
}

// buildSnapshot deep-copies the writer's private windows into a fresh immutable
// snapshot: a new map plus a COPIED samples slice per window. Copying the slice
// (not just aliasing it) is mandatory — a later writer-side append could
// otherwise overwrite array elements a concurrent GetHealth reader is scanning.
func buildSnapshot(windows map[string]*healthWindow) *healthSnapshot {
	m := make(map[string]*healthWindow, len(windows))
	for id, w := range windows {
		samples := make([]healthSample, len(w.samples))
		copy(samples, w.samples)
		m[id] = &healthWindow{providerName: w.providerName, samples: samples}
	}
	return &healthSnapshot{windows: m}
}

// HealthState holds computed health metrics for a provider.
type HealthState struct {
	Status       HealthStatus
	ErrorRate    float64
	AvgLatencyMs int
	SampleCount  int
}

// GetHealth returns the current health state for a provider. It reads the
// immutable snapshot lock-free and applies the 5-min cutoff at READ time — so an
// idle provider whose samples have all aged out is reported Healthy without
// needing a new sample to trigger recovery, matching the prior read-time prune.
// It never writes any field of the shared snapshot (read-only counting).
func (ht *HealthTracker) GetHealth(providerID string) HealthState {
	// snap is always non-nil: NewHealthTracker pre-publishes an empty snapshot
	// before returning, and the tracker is only reachable through it.
	s := ht.snap.Load()
	w := s.windows[providerID]
	if w == nil {
		return HealthState{Status: HealthStatusHealthy}
	}

	cutoff := time.Now().Add(-healthWindowDuration)
	failures := 0
	total := 0
	totalLatency := 0
	for i := range w.samples {
		sm := &w.samples[i]
		if sm.timestamp.Before(cutoff) {
			continue
		}
		total++
		if !sm.success {
			failures++
		}
		totalLatency += sm.latencyMs
	}

	if total == 0 {
		return HealthState{Status: HealthStatusHealthy}
	}

	errorRate := float64(failures) / float64(total)
	avgLatency := totalLatency / total

	status := HealthStatusHealthy
	if errorRate > healthThresholdUnavailable {
		status = HealthStatusUnavailable
	} else if errorRate > healthThresholdDegraded {
		status = HealthStatusDegraded
	}

	return HealthState{
		Status:       status,
		ErrorRate:    errorRate,
		AvgLatencyMs: avgLatency,
		SampleCount:  total,
	}
}

// droppedSamples returns the number of samples the hot path dropped because the
// channel was full — an internal diagnostic (a sustained non-zero value means the
// writer cannot keep up, degrading the advisory health signal). Read by the
// drop-path test; not yet wired to a production metric.
func (ht *HealthTracker) droppedSamples() int64 {
	return ht.dropped.Load()
}

// flush blocks until every sample enqueued before this call has been applied and
// republished into the snapshot. It is a deterministic test seam for
// read-your-write assertions; production never needs it (routing reads health
// during resolve, before the same request records its outcome during execute).
func (ht *HealthTracker) flush() {
	ack := make(chan struct{})
	select {
	case ht.ch <- healthMsg{isFlush: true, ack: ack}:
	case <-ht.stopped:
		return
	}
	select {
	case <-ack:
	case <-ht.stopped:
	}
}
