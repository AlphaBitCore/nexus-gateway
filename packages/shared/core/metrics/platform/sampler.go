package platform

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// Sampler combines L1 runtime samples and L3 business samples (from a
// Registry) into a single SampleBatch per tick. A SampleBatch is the WS
// payload that thingclient pushes to Hub on the metrics_sample channel.
type Sampler struct {
	thingID  string
	rt       *RuntimeSampler
	registry *registry.Registry
}

// NewSampler returns a Sampler bound to the given thingID, process startTime
// (used to derive runtime.uptime_seconds), and a populated Registry.
func NewSampler(thingID string, startTime time.Time, reg *registry.Registry) *Sampler {
	return &Sampler{
		thingID:  thingID,
		rt:       NewRuntimeSampler(startTime),
		registry: reg,
	}
}

// Collect snapshots both surfaces and returns a single batch stamped with
// the current UTC time.
func (s *Sampler) Collect() registry.SampleBatch {
	samples := s.rt.Collect()
	samples = append(samples, s.registry.Collect()...)
	return registry.SampleBatch{
		ThingID:   s.thingID,
		SampledAt: time.Now().UTC(),
		Samples:   samples,
	}
}
