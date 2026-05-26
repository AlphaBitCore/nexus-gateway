package store

import (
	"sync"
	"time"
)

const (
	healthWindowDuration = 5 * time.Minute
	maxHealthSamples     = 100

	healthThresholdDegraded    = 0.05
	healthThresholdUnavailable = 0.25
)

// HealthStatus represents the health state of a provider.
type HealthStatus string

const (
	HealthStatusHealthy     HealthStatus = "healthy"
	HealthStatusDegraded    HealthStatus = "degraded"
	HealthStatusUnavailable HealthStatus = "unavailable"
)

type healthSample struct {
	timestamp time.Time
	success   bool
	latencyMs int
}

// HealthTracker tracks provider health using an in-process sliding window of
// samples. It is used exclusively for per-instance routing decisions (avoiding
// unhealthy providers). Durable health state for the status page is computed
// centrally by the Hub ProviderHealthRollupJob over traffic_event.
type HealthTracker struct {
	mu      sync.Mutex
	windows map[string]*healthWindow // keyed by providerID
}

type healthWindow struct {
	providerName string
	samples      []healthSample
	lastRequest  time.Time
	lastError    *time.Time
}

// NewHealthTracker creates a health tracker.
func NewHealthTracker() *HealthTracker {
	return &HealthTracker{
		windows: make(map[string]*healthWindow),
	}
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
	ht.mu.Lock()
	defer ht.mu.Unlock()

	w := ht.windows[providerID]
	if w == nil {
		w = &healthWindow{providerName: providerName}
		ht.windows[providerID] = w
	}

	now := time.Now()
	w.lastRequest = now
	if !success {
		w.lastError = &now
	}

	w.samples = append(w.samples, healthSample{
		timestamp: now,
		success:   success,
		latencyMs: latencyMs,
	})

	// Cap samples.
	if len(w.samples) > maxHealthSamples {
		w.samples = w.samples[len(w.samples)-maxHealthSamples:]
	}
}

// HealthState holds computed health metrics for a provider.
type HealthState struct {
	Status       HealthStatus
	ErrorRate    float64
	AvgLatencyMs int
	SampleCount  int
}

// GetHealth returns the current health state for a provider.
func (ht *HealthTracker) GetHealth(providerID string) HealthState {
	ht.mu.Lock()
	defer ht.mu.Unlock()

	w := ht.windows[providerID]
	if w == nil {
		return HealthState{Status: HealthStatusHealthy}
	}

	cutoff := time.Now().Add(-healthWindowDuration)
	w.prune(cutoff)

	if len(w.samples) == 0 {
		return HealthState{Status: HealthStatusHealthy}
	}

	failures := 0
	totalLatency := 0
	for _, s := range w.samples {
		if !s.success {
			failures++
		}
		totalLatency += s.latencyMs
	}

	errorRate := float64(failures) / float64(len(w.samples))
	avgLatency := totalLatency / len(w.samples)

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
		SampleCount:  len(w.samples),
	}
}

func (w *healthWindow) prune(cutoff time.Time) {
	i := 0
	for i < len(w.samples) && w.samples[i].timestamp.Before(cutoff) {
		i++
	}
	if i > 0 {
		w.samples = w.samples[i:]
	}
}
