package platform

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

func TestSamplerCombinesL1AndL3(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := registry.NewRegistry(reg)
	c := r.NewCounter("test.events_total", []string{})
	c.With().Inc()

	s := NewSampler("thing-abc", time.Now().Add(-time.Hour), r)
	batch := s.Collect()

	if batch.ThingID != "thing-abc" {
		t.Errorf("thingID = %q", batch.ThingID)
	}
	if len(batch.Samples) < 12 { // 11 runtime + >=1 business
		t.Errorf("expected >= 12 samples, got %d", len(batch.Samples))
	}
}
