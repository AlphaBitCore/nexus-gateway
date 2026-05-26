package wiring

import (
	"fmt"
	"log/slog"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/spilluploader"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/intercept"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform"
	policy "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// InitRelay creates the outbound HTTP relay client.
// Singleton — per-host HTTP/2 pool absorbs concurrency to a single provider host.
func InitRelay(opsReg *registry.Registry, version string) (*relay.Client, error) {
	return relay.New(relay.Config{
		UserAgent:   fmt.Sprintf("nexus-agent/%s", version),
		OpsRegistry: opsReg,
	})
}

// InitPlatform creates the platform-specific network interception shim
// (macOS NE / Linux iptables / Windows NexusWFP).
func InitPlatform(bridgeAddr string, relayClient *relay.Client) platform.Platform {
	return platform.NewPlatform(bridgeAddr, relayClient)
}

// ConnectionBridgeConfig groups everything InitConnectionBridge needs.
type ConnectionBridgeConfig struct {
	PolicyEngine        *policy.Engine
	InterceptHandler    *intercept.Handler
	AgentPipeline       *agentcompliance.AgentPipeline
	AuditQueue          *auditqueue.Queue
	ThingID             string
	PayloadCaptureStore *payloadcapture.Store
	SpillUploader       *spilluploader.Uploader
	KillSwitch          *killswitch.Switch
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
		InterceptHandler:        cfg.InterceptHandler,
		AgentPipeline:           cfg.AgentPipeline,
		AuditQueue:              cfg.AuditQueue,
		ThingID:                 cfg.ThingID,
		PayloadCaptureStore:     cfg.PayloadCaptureStore,
		SpillUploader:           cfg.SpillUploader,
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
