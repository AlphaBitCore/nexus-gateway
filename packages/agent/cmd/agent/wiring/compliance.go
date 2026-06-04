package wiring

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	policy "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/policies"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// ComplianceConfig is the subset of agent config needed for the
// compliance + policy subsystem.
type ComplianceConfig struct {
	DefaultAction             string
	ExemptionEnabled          bool
	ExemptionFailureThreshold int
	ExemptionWindowSec        int
	ExemptionDurationSec      int
	ExemptionAllowlist        []string
	ExemptionDenylist         []string
	// LocalBodyCapture seeds the always-on local capture store (see
	// ComplianceBundle.LocalCaptureStore). Default-true is applied by the
	// agent config loader.
	LocalBodyCapture bool
}

// ComplianceBundle groups all compliance subsystem objects.
type ComplianceBundle struct {
	PolicyEngine   *policy.Engine
	ExemptionStore *exemption.Store
	AgentPipeline  *agentcompliance.AgentPipeline
	// PayloadCaptureStore holds the Hub-pushed payload_capture config. On the
	// agent it is the UPLOAD gate (StoreRequestBody/StoreResponseBody decide
	// whether a captured body is shipped to Hub/S3 at drain time) plus the
	// source of the size params.
	PayloadCaptureStore *payloadcapture.Store
	// LocalCaptureStore drives the inspect path's local body capture (tlsbump).
	// Always-on by default (LocalBodyCapture), independent of the Hub config,
	// so users always see their own AI traffic locally. Mirrors the server
	// store's size params; re-derived on every Hub payload_capture push.
	LocalCaptureStore    *payloadcapture.Store
	StreamingPolicyStore *streampolicy.Store
	PoliciesCache        *policies.SnapshotCache
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

	// Payload capture Store (the UPLOAD gate on the agent). Boots with the
	// zero-risk default (all-off) — the first Hub shadow push of
	// "payload_capture" swaps in the admin-configured upload flags + size params.
	payloadCaptureStore := payloadcapture.NewStore(payloadcapture.DefaultConfig())

	// Local capture store (the always-on local-capture gate for tlsbump).
	// Flags follow LocalBodyCapture (default true); size params mirror the
	// server store and are re-derived on every Hub push (see SyncLocalCapture).
	localCaptureCfg := payloadcapture.DefaultConfig()
	localCaptureCfg.StoreRequestBody = cfg.LocalBodyCapture
	localCaptureCfg.StoreResponseBody = cfg.LocalBodyCapture
	localCaptureStore := payloadcapture.NewStore(localCaptureCfg)

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
		PayloadCaptureStore:  payloadCaptureStore,
		LocalCaptureStore:    localCaptureStore,
		StreamingPolicyStore: streamingPolicyStore,
		PoliciesCache:        policiesCache,
	}
}

// SyncLocalCapture re-derives the always-on local capture config from the
// freshly-applied server config: it keeps the server's size params (inline
// cutoff + read caps, which the Hub push may have changed) but overrides the
// capture flags with the agent-local LocalBodyCapture intent. Called by the
// payload_capture applier after each Hub push so the local capture store
// tracks server param changes without ever letting the Hub disable local
// capture. No-op when localStore is nil.
func SyncLocalCapture(localStore *payloadcapture.Store, serverCfg payloadcapture.Config, localBodyCapture bool) {
	if localStore == nil {
		return
	}
	cfg := serverCfg
	cfg.StoreRequestBody = localBodyCapture
	cfg.StoreResponseBody = localBodyCapture
	localStore.Set(cfg)
}

// TeeApplier wraps an inner ShadowApplier so every apply is also
// recorded in the policies cache (for the GET_APPLIED_CONFIG IPC).
func TeeApplier(key string, inner shadow.ShadowApplier, cache *policies.SnapshotCache) shadow.ShadowApplier {
	return policies.TeeApplier{Inner: inner, Cache: cache, CfgKey: key}
}
