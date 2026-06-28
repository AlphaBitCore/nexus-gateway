package wiring

import (
	"context"
	"log/slog"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform"
	policy "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/core"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
)

// InitPlatform creates the platform-specific network interception shim
// (macOS NE / Linux iptables / Windows NexusWFP).
func InitPlatform(bridgeAddr string) platform.Platform {
	return platform.NewPlatform(bridgeAddr)
}

// ConnectionBridgeConfig groups everything InitConnectionBridge needs.
type ConnectionBridgeConfig struct {
	PolicyEngine  *policy.Engine
	AgentPipeline *agentcompliance.AgentPipeline
	AuditQueue    *auditqueue.Queue
	ThingID       string
	KillSwitch    *killswitch.Switch
	// InspectBodyCap is the per-flow buffer ceiling (default 256 MiB).
	InspectBodyCap          int64
	ProviderTrafficNotifier func()
}

// InitConnectionBridge creates the ConnectionBridge that routes
// platform-intercepted connections through the policy engine and records
// audit events on completion.
func InitConnectionBridge(cfg ConnectionBridgeConfig) *ConnectionBridge {
	const defaultInspectBodyCap int64 = 256 * 1024 * 1024
	cap := cfg.InspectBodyCap
	if cap <= 0 {
		cap = defaultInspectBodyCap
	}
	return &ConnectionBridge{
		PolicyEngine:            cfg.PolicyEngine,
		AgentPipeline:           cfg.AgentPipeline,
		AuditQueue:              cfg.AuditQueue,
		ThingID:                 cfg.ThingID,
		KillSwitch:              cfg.KillSwitch,
		InspectBodyCap:          cap,
		ProviderTrafficNotifier: cfg.ProviderTrafficNotifier,
	}
}

// WireBackpressure wires the backpressure store into the darwin platform shim.
// No-op on Linux/Windows (those platforms don't have the bridge ingress yet).
// Delegates to the per-OS wireDarwinBackpressure function in cmd/agent/.
type BackpressureWirer interface {
	WireBackpressure(plat platform.Platform)
}

// WireInterceptionHealth wires the InterceptionHealth reporter from the platform
// into the status collector's health function.
type InterceptionHealthSetter interface {
	SetInterceptionHealthFn(fn func() InterceptionHealth)
}

// InterceptionHealth is the status shape for the interception subsystem.
type InterceptionHealth struct {
	StartedAt        interface{}
	Connected        bool
	ConnectionsTotal int64
	ActiveSessions   int64
	LastFlowAt       interface{}
}

// LogPlatformStartup logs the platform's interception mode via slog.
func LogPlatformStartup(plat platform.Platform, logger *slog.Logger) {
	if r, ok := plat.(platform.InterceptionModeReporter); ok {
		logger.Info("platform interception mode", "mode", string(r.InterceptionMode()))
	}
}

// WireQUICFallback pushes the Hub-configured force-QUIC-TCP-fallback image
// allowlist (config key forceQUICFallbackBundles) into the platform and keeps
// it in sync on every config change. No-op for platforms that don't implement
// ForceQUICFallbackController: macOS drives the same Hub key through its NE
// file-based path, and Linux has no QUIC handling, so only Windows reacts.
func WireQUICFallback(ctx context.Context, plat platform.Platform, cfgMgr *config.Manager, logger *slog.Logger) {
	ctrl, ok := plat.(platform.ForceQUICFallbackController)
	if !ok {
		return
	}
	// Initial push from current config; the platform stores it and re-pushes
	// once the kernel driver is up if it isn't already.
	ctrl.SetForceQUICFallbackImages(cfgMgr.Get().ForceQUICFallbackBundles)

	ch := cfgMgr.Subscribe()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				ctrl.SetForceQUICFallbackImages(cfgMgr.Get().ForceQUICFallbackBundles)
			}
		}
	}()
	logger.Info("QUIC-fallback controller wired (Windows NexusWFP UDP/443 block)")
}

// StartPlatformInterception launches the platform interception accept loop
// in a recovered goroutine. A start failure is fail-open: the agent keeps
// running without interception and logs a warning.
func StartPlatformInterception(ctx context.Context, plat platform.Platform, connHandler *ConnectionBridge, recoveryCfg shareddiag.RecoveryConfig) {
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "platform-intercept"
		defer shareddiag.Recover(rcfg, nil)
		if err := plat.Start(ctx, connHandler); err != nil {
			slog.Warn("platform interception not available", "error", err)
		}
	}()
}
