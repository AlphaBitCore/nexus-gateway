// infra.go initialises transport-layer infrastructure: access control,
// connection manager, upstream transport, and the adapter registry.
package wiring

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// InfraResult bundles transport-layer infrastructure.
type InfraResult struct {
	AccessChecker     *access.Checker
	ConnManager       *conn.Manager
	ShutdownCoord     *conn.ShutdownCoordinator
	UpstreamTransport *tlsbump.UpstreamTransport
	AdapterRegistry   *traffic.AdapterRegistry
}

// InitInfra initialises access control, connection management, upstream
// transport, and the adapter registry. Returns an error if any component
// fails to start.
func InitInfra(cfg *config.Config, logger *slog.Logger) (InfraResult, error) {
	accessChecker, err := access.NewChecker(
		cfg.AccessControl.SourceIPAllowlist,
		cfg.AccessControl.DomainAllowlist,
		cfg.AccessControl.InternalNetworkExceptions,
	)
	if err != nil {
		return InfraResult{}, fmt.Errorf("access control: %w", err)
	}
	slog.Info("access control initialized",
		"sourceIPs", len(cfg.AccessControl.SourceIPAllowlist),
		"domains", len(cfg.AccessControl.DomainAllowlist))
	if cfg.AccessControl.AllowUnlistedPassthrough {
		slog.Warn("unlisted-passthrough ENABLED — CONNECTs to non-allowlisted domains tunneled as raw TCP without audit (DEV ONLY)")
	}

	connManager := conn.NewManager(cfg.Connections.MaxConcurrentTunnels)
	shutdownCoord := conn.NewShutdownCoordinator(
		ParseDurationOrDefault(cfg.Connections.ShutdownGracePeriod, 30*time.Second),
		logger,
	)

	upstream, err := tlsbump.NewUpstreamTransport(
		cfg.Upstream.MaxConnsPerHost,
		ParseDurationOrDefault(cfg.Upstream.IdleConnTimeout, 90*time.Second),
		ParseDurationOrDefault(cfg.Upstream.DialTimeout, 10*time.Second),
	)
	if err != nil {
		return InfraResult{}, fmt.Errorf("upstream transport: %w", err)
	}

	adapterRegistry := traffic.NewAdapterRegistry("nexus_compliance_proxy")
	adapters.RegisterBuiltins(adapterRegistry)
	adapterRegistry.Freeze()

	return InfraResult{
		AccessChecker:     accessChecker,
		ConnManager:       connManager,
		ShutdownCoord:     shutdownCoord,
		UpstreamTransport: upstream,
		AdapterRegistry:   adapterRegistry,
	}, nil
}
