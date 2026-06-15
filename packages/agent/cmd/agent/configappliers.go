package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/protectionpause"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/policies"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/platformshim"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/wiring"
)

// configAppliers holds all the shadow key applier closures and derived objects.
type configAppliers struct {
	// Per-key appliers for configDispatchDeps.
	exemptions          rawApply
	killSwitchApply     rawApply
	agentSettings       rawApply
	diagMode            rawApply
	interceptionDomains rawApply
	hookConfig          rawApply
	payloadCapture      rawApply
	streamingCompliance rawApply
	installedRulePacks  rawApply
	userContext         rawApply

	// Objects derived during applier construction.
	killSwitchObj *killswitch.Switch
	pauser        *protectionpause.Pauser
	diagModeLevel *diagModeLevelController
}

// buildConfigAppliersArgs carries the dependencies for buildConfigAppliers.
type buildConfigAppliersArgs struct {
	cfg                 *config.AgentConfig
	cfgMgr              *config.Manager
	logger              *slog.Logger
	statusCollectorPtr  **status.Collector
	agentPipeline       *agentcompliance.AgentPipeline
	exemptionStore      *exemption.Store
	payloadCaptureStore *payloadcapture.Store
	// localCaptureStore is the always-on local-capture store fed to tlsbump.
	// The payload_capture applier re-derives it (server params + local flags)
	// on every Hub push via wiring.SyncLocalCapture. localBodyCapture is the
	// agent-local capture intent (yaml localBodyCapture, default true).
	localCaptureStore    *payloadcapture.Store
	localBodyCapture     bool
	streamingPolicyStore *streampolicy.Store
	policiesCache        *policies.SnapshotCache
	// attestationEnabled is the live boolean read by the agent's
	// tlsbump request injector before stamping every outbound HTTPS
	// request with X-Nexus-Attestation. The agent_settings shadow
	// applier writes it; the attestation Signer reads it on every
	// Sign call so admin toggles propagate without a daemon restart.
	// Nil means attestation is not wired in this build — applier
	// no-ops, Signer fail-opens (omits header).
	attestationEnabled *atomic.Bool
}

func applyOf(a shadow.ShadowApplier) rawApply {
	return func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, a.ApplyShadowState(ctx, raw)
	}
}

// applyOfKillSwitch builds a rawApply for the kill-switch shadow key that:
//   - Drives the kill switch exclusively through pauser.EngageAdmin /
//     pauser.DisengageAdmin so a user-initiated Resume never accidentally
//     disengages a Hub-pushed fleet brake.
//   - Returns the live SnapshotState rather than nil so Hub echoes the
//     actually-applied state instead of the raw desired payload.
//
// Fail-safe: an empty or null payload is a no-op — the admin-brake state
// is preserved unchanged.  This mirrors killswitch.ApplyShadowState semantics
// but goes through the pauser so the two-source (admin/user) logic is
// honoured.
func applyOfKillSwitch(ks *killswitch.Switch, pauser *protectionpause.Pauser) rawApply {
	return func(_ context.Context, raw []byte, _ int64) ([]byte, error) {
		if len(raw) > 0 && string(raw) != "null" {
			// Decode the wire payload. Use a pointer so we can detect a missing
			// "engaged" field (present-vs-absent) — an explicit false must
			// disengage, but an absent field is treated as a no-op.
			var payload struct {
				Engaged *bool `json:"engaged"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, fmt.Errorf("parse killswitch shadow: %w", err)
			}
			if payload.Engaged != nil {
				if *payload.Engaged {
					pauser.EngageAdmin("hub-shadow")
				} else {
					pauser.DisengageAdmin("hub-shadow")
				}
			}
			// If payload.Engaged == nil ({} or no "engaged" field) — no-op,
			// preserve current admin-brake state.
		}
		// Return the live snapshot so Hub sees the actually-applied state.
		b, err := json.Marshal(ks.SnapshotState())
		if err != nil {
			return nil, fmt.Errorf("marshal killswitch snapshot: %w", err)
		}
		return b, nil
	}
}

func adaptApplyFunc(fn func(ctx context.Context, raw json.RawMessage) error) rawApply {
	return func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, fn(ctx, raw)
	}
}

func teeCatB(key string, inner shadow.ShadowApplier, cache *policies.SnapshotCache) shadow.ShadowApplier {
	return policies.TeeApplier{Inner: inner, Cache: cache, CfgKey: key}
}

// buildConfigAppliers constructs all shadow key applier closures. This is
// called once in cmdRun after all the subsystem objects have been created.
func buildConfigAppliers(a buildConfigAppliersArgs) configAppliers {
	// Kill switch + pauser (needed by agentSettings applier + network layer).
	ks := killswitch.New(a.logger)
	pauser := protectionpause.New(ks)

	// Diag-mode log-level controller. The agent_settings applier drives it
	// from diagModeUntil: it raises the local log level to debug for the
	// window and self-restores the startup baseline on expiry. Baseline is
	// the level the agent booted with (cfg.Log.Level; empty resolves to info).
	diagLevelCtl := newDiagModeLevelController(a.cfg.Log.Level, a.logger)

	agentSettingsApply := adaptApplyFunc(func(_ context.Context, raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		var as struct {
			QuitAllowed              *bool             `json:"quitAllowed"`
			ShutdownWarning          map[string]string `json:"shutdownWarning"`
			ShutdownWarningEnabled   bool              `json:"shutdownWarningEnabled"`
			TrafficUploadLevel       string            `json:"trafficUploadLevel"`
			ForceQUICFallbackBundles []string          `json:"forceQUICFallbackBundles"`
			BypassBundles            []string          `json:"bypassBundles"`
			// Fleet toggle for traffic attestation. When true and an Ed25519
			// cert is on disk, the request injector stamps X-Nexus-Attestation
			// on every outbound HTTPS request.
			AttestationEnabled bool `json:"attestationEnabled"`
		}
		if err := json.Unmarshal(raw, &as); err != nil {
			return fmt.Errorf("decode agent_settings state: %w", err)
		}
		remoteOverlay := map[string]any{}
		if as.QuitAllowed != nil {
			remoteOverlay["quitAllowed"] = *as.QuitAllowed
		}
		if as.TrafficUploadLevel != "" {
			remoteOverlay["trafficUploadLevel"] = as.TrafficUploadLevel
		}
		if as.ForceQUICFallbackBundles != nil {
			remoteOverlay["forceQUICFallbackBundles"] = platformshim.AnySlice(as.ForceQUICFallbackBundles)
		}
		if as.BypassBundles != nil {
			remoteOverlay["bypassBundles"] = platformshim.AnySlice(as.BypassBundles)
		}
		if len(remoteOverlay) > 0 {
			next := config.MergeConfig(a.cfgMgr.Get(), remoteOverlay)
			a.cfgMgr.Swap(next)
		}
		if as.ForceQUICFallbackBundles != nil {
			if err := platformshim.WriteQUICFallbackBundlesFile(as.ForceQUICFallbackBundles, a.logger); err != nil {
				a.logger.Warn("write quic-bundles.json failed", "error", err, "count", len(as.ForceQUICFallbackBundles))
			}
		}
		if as.BypassBundles != nil {
			if err := platformshim.WriteBypassBundlesFile(as.BypassBundles, a.logger); err != nil {
				a.logger.Warn("write bypass-bundles.json failed", "error", err, "count", len(as.BypassBundles))
			}
		}
		if sc := *a.statusCollectorPtr; sc != nil {
			if as.ShutdownWarningEnabled && as.ShutdownWarning != nil {
				sc.SetShutdownWarning(as.ShutdownWarning)
			} else {
				sc.SetShutdownWarning(nil)
			}
		}
		// Propagate attestationEnabled to the runtime atomic so the
		// Signer's enabled-lookup closure sees the live value on the
		// next outbound request. Nil flag is a no-op.
		if a.attestationEnabled != nil {
			a.attestationEnabled.Store(as.AttestationEnabled)
		}
		a.logger.Info("agent_settings shadow applied",
			"quitAllowedSet", as.QuitAllowed != nil,
			"shutdownWarningEnabled", as.ShutdownWarningEnabled,
			"trafficUploadLevel", as.TrafficUploadLevel,
			"attestationEnabled", as.AttestationEnabled,
		)
		return nil
	})

	// diag_mode applier. The per-thing diag_mode override carries {until} —
	// an RFC3339 timestamp; an empty or cleared override means no window. It
	// drives the log-level controller: raise to debug for the window, restore
	// the baseline on expiry.
	diagModeApply := adaptApplyFunc(func(_ context.Context, raw json.RawMessage) error {
		var s struct {
			Until string `json:"until"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("decode diag_mode state: %w", err)
			}
		}
		diagLevelCtl.apply(s.Until)
		return nil
	})

	return configAppliers{
		exemptions:          applyOf(teeCatB("exemptions", a.exemptionStore, a.policiesCache)),
		killSwitchApply:     applyOfKillSwitch(ks, pauser),
		agentSettings:       agentSettingsApply,
		diagMode:            diagModeApply,
		interceptionDomains: applyOf(teeCatB("interception_domains", shadow.AdapterFunc(a.agentPipeline.ApplyDomainsShadowState), a.policiesCache)),
		hookConfig:          applyOf(teeCatB("hooks", shadow.AdapterFunc(a.agentPipeline.ApplyHooksShadowState), a.policiesCache)),
		payloadCapture: applyOf(teeCatB("payload_capture", shadow.AdapterFunc(func(ctx context.Context, raw json.RawMessage) error {
			// Apply the Hub config to the server store (the upload gate +
			// size-param source), then re-derive the always-on local capture
			// store: server params, local flags. A Hub push can change the
			// inline cutoff but can never disable local capture.
			if err := a.payloadCaptureStore.ApplyShadowState(ctx, raw); err != nil {
				return err
			}
			wiring.SyncLocalCapture(a.localCaptureStore, a.payloadCaptureStore.Get(), a.localBodyCapture)
			return nil
		}), a.policiesCache)),
		streamingCompliance: applyOf(teeCatB("streaming_compliance", shadow.AdapterFunc(a.streamingPolicyStore.ApplyShadowState), a.policiesCache)),
		installedRulePacks: applyOf(teeCatB("installed_rule_packs", shadow.AdapterFunc(func(ctx context.Context, raw json.RawMessage) error {
			return a.agentPipeline.ApplyRulePacksShadowState(ctx, raw)
		}), a.policiesCache)),
		userContext: applyOf(teeCatB("user_context", shadow.AdapterFunc(func(_ context.Context, _ json.RawMessage) error {
			return nil
		}), a.policiesCache)),
		killSwitchObj: ks,
		pauser:        pauser,
		diagModeLevel: diagLevelCtl,
	}
}
