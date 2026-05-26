package wiring

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// OpsRegistryResult holds the ops metrics registry and start time.
type OpsRegistryResult struct {
	Registry         *registry.Registry
	ProcessStartTime time.Time
}

// InitOpsRegistry creates the opsmetrics registry backed by
// prometheus.DefaultRegisterer and registers all compliance-proxy
// instrument bindings up-front so subsystems can record values safely
// regardless of init order.
func InitOpsRegistry() OpsRegistryResult {
	reg := registry.NewRegistry(prometheus.DefaultRegisterer)
	cpmetrics.Register(reg)
	tlsbump.RegisterMetrics(reg)
	cache.Register(reg)
	audit.Register(reg)
	return OpsRegistryResult{
		Registry:         reg,
		ProcessStartTime: time.Now().UTC(),
	}
}
