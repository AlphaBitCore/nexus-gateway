package wiring

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/intercept"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/policies"
	policy "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// ComplianceConfig is the subset of agent config needed for the
// compliance + policy subsystem.
type ComplianceConfig struct {
	DefaultAction            string
	ExemptionEnabled         bool
	ExemptionFailureThreshold int
	ExemptionWindowSec       int
	ExemptionDurationSec     int
	ExemptionAllowlist       []string
	ExemptionDenylist        []string
}

// ComplianceBundle groups all compliance subsystem objects.
type ComplianceBundle struct {
	PolicyEngine        *policy.Engine
	ExemptionStore      *exemption.Store
	AgentPipeline       *agentcompliance.AgentPipeline
	InterceptHandler    *intercept.Handler
	PayloadCaptureStore *payloadcapture.Store
	StreamingPolicyStore *streampolicy.Store
	PoliciesCache       *policies.SnapshotCache
}

// InitCompliance builds the policy engine, exemption store, agent
// compliance pipeline, intercept handler, and payload/streaming stores.
func InitCompliance(cfg ComplianceConfig, logger *slog.Logger) ComplianceBundle {
	// Policy engine
	policyEngine := policy.NewEngine(cfg.DefaultAction)

	// Exemption store (TLS bump auto-exemption).
	exemptionStore := exemption.NewStore(exemption.Config{
		Enabled:              cfg.ExemptionEnabled,
		FailureThreshold:     cfg.ExemptionFailureThreshold,
		WindowSeconds:        cfg.ExemptionWindowSec,
		ExemptionDurationSec: cfg.ExemptionDurationSec,
	})
	exemptionStore.SetAllowlist(cfg.ExemptionAllowlist)
	exemptionStore.SetDenylist(cfg.ExemptionDenylist)
	policyEngine.SetExemptionStore(exemptionStore)

	// V2 Hook pipeline (shared compliance)
	agentPipeline := agentcompliance.NewAgentPipeline(logger)
	interceptHandler := intercept.NewHandler(agentPipeline, logger)

	// Wire interception_domains into the policy engine via the
	// AgentPipeline's domain snapshot.
	policyEngine.SetInterceptionHostsFn(func() []string {
		snap := agentPipeline.Snapshot()
		if snap == nil {
			return nil
		}
		return snap.HostPatterns()
	})

	// Register shared-hooks regex cache counters on the default registerer.
	core.RegisterRegexCacheMetrics(prometheus.DefaultRegisterer)

	// Payload capture Store. Boots with the zero-risk default (all-off,
	// 64 KiB cap) — the first Hub shadow push of "payload_capture" swaps
	// in the admin-configured values.
	payloadCaptureStore := payloadcapture.NewStore(payloadcapture.DefaultConfig())

	// Streaming compliance policy Store. Agent has no local config DB —
	// every admin policy arrives via Hub shadow push, so BootStore with
	// a nil loader installs DefaultPolicy() and waits for the first
	// shadow_compliance push (handled via ApplyShadowState). Sharing
	// the BootStore helper with compliance-proxy and ai-gateway means
	// the same log shape + defaulting semantics hold for every service
	// (#115 three-end alignment). Context.Background() is safe because
	// BootStore's nil-loader path never invokes the loader.
	streamingPolicyStore := streampolicy.BootStore(context.Background(), nil, logger)

	// Policies snapshot cache: mirrors the raw payload each Cat B applier
	// accepted so the Dashboard's GET_APPLIED_CONFIG IPC can render
	// authoritative rows.
	policiesCache := policies.NewSnapshotCache()

	return ComplianceBundle{
		PolicyEngine:         policyEngine,
		ExemptionStore:       exemptionStore,
		AgentPipeline:        agentPipeline,
		InterceptHandler:     interceptHandler,
		PayloadCaptureStore:  payloadCaptureStore,
		StreamingPolicyStore: streamingPolicyStore,
		PoliciesCache:        policiesCache,
	}
}

// TeeApplier wraps an inner ShadowApplier so every apply is also
// recorded in the policies cache (for the GET_APPLIED_CONFIG IPC).
func TeeApplier(key string, inner shadow.ShadowApplier, cache *policies.SnapshotCache) shadow.ShadowApplier {
	return policies.TeeApplier{Inner: inner, Cache: cache, CfgKey: key}
}
