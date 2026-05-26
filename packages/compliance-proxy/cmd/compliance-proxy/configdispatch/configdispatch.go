// Package configdispatch wires every shadow config key compliance-proxy
// consumes onto a single shared/transport/configloader.Loader.
//
// Each registration takes a single per-key Parse + Apply pair; the
// Loader handles outcome tracking, error wrapping, reported-map
// assembly, and structured logging. Adding a new shadow key is a one-
// place edit here — no further plumbing in main.go.
package configdispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	cpconfigloader "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/loaders"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	proxyserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/server"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// HookConfigReloader is the narrow surface configdispatch needs from the
// hot-reloadable HookConfig cache. The full interface lives in
// internal/compliance — only Reload matters here, so we accept any value
// satisfying the contract (the real one is *compliance.HookConfigCache).
type HookConfigReloader interface {
	Reload(ctx context.Context) error
}

// Deps carries every subsystem the per-key handlers touch. Pulling them
// through a single struct keeps the Register chain readable and forces
// callers (currently only main.go) to be explicit about the wiring contract.
type Deps struct {
	Logger              *slog.Logger
	ThingID             string
	Outcomes            *thingclient.OutcomeTracker
	KillSwitch          *killswitch.KillSwitch
	ExemptionStore      *exemption.Store
	HookConfigCache      HookConfigReloader // may be nil
	ConfigDB             *sql.DB            // may be nil
	CacheManager         *cache.Manager
	AccessChecker        *access.Checker
	TelemetryProvider    *telemetry.SwappableTracerProvider // may be nil
	PayloadCaptureStore  *payloadcapture.Store
	StreamingPolicyStore *streampolicy.Store // #115 — Hub shadow handler routes here via ApplyShadowState
	ProxyServer          *proxyserver.ProxyServer
}

// HubAndLoaderResult is returned by InitHubAndCfgLoader.
type HubAndLoaderResult struct {
	// ThingClient is nil when Hub is not configured or startup failed.
	ThingClient *thingclient.Client
	// CfgLoader is the fully wired Loader (with OutcomeTracker when
	// ThingClient is non-nil; nil-safe when ThingClient is nil).
	CfgLoader *cfgloader.Loader
}

// InitHubAndCfgLoader handles the two-phase setup required to get a live
// OutcomeTracker into the config Loader: first build with nil Outcomes so the
// OnConfigChanged callback closure can reference cfgLoader; then start
// thingclient; then rebuild with tc.Outcomes() so future apply calls report
// outcomes correctly. The caller supplies a factory func so that
// configdispatch never needs to import the wiring package.
func InitHubAndCfgLoader(
	ctx context.Context,
	base Deps,
	tcFactory func(onConfigChanged func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)) *thingclient.Client,
) HubAndLoaderResult {
	// Phase 1: build loader with nil outcomes so the closure captures a
	// stable pointer that works before tc starts.
	loader := BuildConfigLoader(base)

	apply := func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		reported, applyErr := loader.Apply(ctx, desired)
		for k, cs := range desired {
			if !loader.Has(k) {
				reported[k] = cs
			}
		}
		return reported, applyErr
	}

	tc := tcFactory(apply)
	if tc == nil {
		return HubAndLoaderResult{ThingClient: nil, CfgLoader: loader}
	}

	// Phase 2: rebuild loader with live OutcomeTracker now that tc exists.
	base.Outcomes = tc.Outcomes()
	loader = BuildConfigLoader(base)
	return HubAndLoaderResult{ThingClient: tc, CfgLoader: loader}
}

// BuildConfigLoader returns a Loader pre-populated with every shadow
// key compliance-proxy consumes. Construction is pure — no I/O, no
// timers — so tests can build and exercise it without spinning a Hub.
func BuildConfigLoader(d Deps) *cfgloader.Loader {
	l := cfgloader.New(d.Logger, d.Outcomes, d.ThingID, "compliance-proxy")

	registerKillSwitch(l, d)
	registerExemptions(l, d)
	registerHookConfig(l, d)
	registerInterceptionDomains(l, d)
	registerObservability(l, d)
	registerPayloadCapture(l, d)
	registerStreamingCompliance(l, d)
	registerOnboarding(l, d)
	registerLogLevel(l, d)

	return l
}

// reloadAllowlistAndSwap is the body invoked by the "interception_domains"
// receiver: invalidate the allowlist category, ask the cache manager to
// re-materialise it, and swap it onto the access checker. The interception
// path additionally invalidates the rich-row category before triggering
// the reload.
func reloadAllowlistAndSwap(ctx context.Context, d Deps) error {
	if d.CacheManager == nil || d.ConfigDB == nil {
		return nil
	}
	reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := d.CacheManager.Get(reloadCtx, cache.CategoryAllowlists)
	if err != nil {
		return fmt.Errorf("reload allowlist: %w", err)
	}
	if entries, ok := data.([]string); ok {
		d.AccessChecker.SwapDomainAllowlist(entries, d.Logger)
	}
	return nil
}

type killswitchState struct {
	Engaged bool `json:"engaged"`
}

func registerKillSwitch(l *cfgloader.Loader, d Deps) {
	cfgloader.Register(l, cfgloader.Handler[killswitchState]{
		Key:   "killswitch",
		Parse: cfgloader.ParseJSON[killswitchState](),
		Apply: func(ctx context.Context, v killswitchState, ver int64) ([]byte, error) {
			if v.Engaged != d.KillSwitch.IsEngaged() {
				d.KillSwitch.Toggle(v.Engaged, "hub-shadow")
			}
			// Report the LIVE snapshot rather than echoing desired:
			// a local rejection (or a lagging shadow tick) must surface
			// the actually-applied state to Hub, otherwise the Nodes
			// page shows a false "in sync".
			snap := d.KillSwitch.Snapshot()
			b, err := json.Marshal(snap)
			if err != nil {
				return nil, fmt.Errorf("build snapshot: %w", err)
			}
			return b, nil
		},
	})
}

// registerExemptions wires the `exemptions` Type B (Cat B) shadow
// receiver. CP fires the invalidation with an empty state; the handler
// re-reads compliance_exemption_grant directly via cpconfigloader and
// pushes the canonical wire shape into the in-memory ExemptionStore via
// Rebuild.
//
// The exemptions key uses Category B (pull-on-signal): CP fires the
// invalidation with an empty state and re-reads the DB on demand. This
// avoids the ~5666 redundant WebSocket push events per day that a
// Category A (push-full-snapshot) approach generated at idle.
//
// ConfigDB may be nil in tests / minimal embeds (the handler is tolerant
// — a nil DB just means we skip the reload). LoadActiveExemptions itself
// also short-circuits on nil DB, so the guard is defence-in-depth.
func registerExemptions(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.Exemptions, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.ConfigDB == nil {
			return nil, nil
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		grants, err := cpconfigloader.LoadActiveExemptions(reloadCtx, d.ConfigDB)
		if err != nil {
			return nil, fmt.Errorf("reload exemptions: %w", err)
		}
		d.ExemptionStore.Rebuild(grants)
		d.Logger.Info("exemptions reloaded from DB",
			"active", len(grants),
		)
		return nil, nil
	})
}

func registerHookConfig(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.Hooks, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// hookConfigCache may be nil when compliance init disabled it;
		// the cache invalidate path stays best-effort regardless.
		var reloadErr error
		if d.HookConfigCache != nil {
			reloadErr = d.HookConfigCache.Reload(ctx)
		}
		if d.CacheManager != nil {
			d.CacheManager.Invalidate(cache.CategoryHooks)
		}
		return nil, reloadErr
	})
}

func registerInterceptionDomains(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "interception_domains", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.CacheManager != nil {
			d.CacheManager.Invalidate(cache.CategoryInterceptionDomains)
			d.CacheManager.Invalidate(cache.CategoryAllowlists)
		}
		return nil, reloadAllowlistAndSwap(ctx, d)
	})
}

func registerObservability(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "observability", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.CacheManager == nil || d.TelemetryProvider == nil {
			return nil, nil
		}
		d.CacheManager.Invalidate(cache.CategoryObservability)
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		data, err := d.CacheManager.Get(reloadCtx, cache.CategoryObservability)
		if err != nil {
			return nil, fmt.Errorf("reload observability: %w", err)
		}
		otelCfg, ok := data.(*telemetry.Config)
		if !ok || otelCfg == nil {
			return nil, nil
		}
		if err := d.TelemetryProvider.Reconfigure(*otelCfg); err != nil {
			return nil, fmt.Errorf("reconfigure telemetry: %w", err)
		}
		return nil, nil
	})
}

func registerPayloadCapture(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, "payload_capture", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.ConfigDB == nil {
			return nil, nil
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		pcCfg, err := cpconfigloader.LoadPayloadCaptureConfig(reloadCtx, d.ConfigDB)
		if err != nil {
			return nil, fmt.Errorf("reload payload capture: %w", err)
		}
		d.PayloadCaptureStore.Set(pcCfg)
		d.Logger.Info("payload capture config reloaded",
			"storeRequestBody", pcCfg.StoreRequestBody,
			"storeResponseBody", pcCfg.StoreResponseBody,
			"maxInlineBodyBytes", pcCfg.MaxInlineBodyBytes,
		)
		return nil, nil
	})
}

// registerStreamingCompliance wires the `streaming_compliance` Type B
// shadow receiver. CP fires the invalidation with an empty state; the
// handler re-reads system_metadata['streaming_compliance.config'] via
// the canonical streampolicy.LoadGlobalDefault loader and pushes the
// resulting Policy onto the live ProxyServer via SetStreamingPolicyGlobal.
//
// ConfigDB may be nil in tests / minimal embeds (the handler is
// tolerant — a nil DB just means we skip the reload). ProxyServer may
// also be nil in unit-level loader tests; the handler tolerates that
// too so BuildConfigLoader can be invoked from a thin test harness.
func registerStreamingCompliance(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.StreamingCompliance, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		// #115 — Hub now pushes the raw blob; the Store applies it via
		// the canonical ApplyShadowState path. No DB re-read or per-server
		// setter wrapper needed; Store.Get() readers on the hot path see
		// the new policy atomically.
		if d.StreamingPolicyStore == nil {
			return nil, nil
		}
		if err := d.StreamingPolicyStore.ApplyShadowState(ctx, raw); err != nil {
			return nil, fmt.Errorf("apply streaming compliance shadow state: %w", err)
		}
		policy := d.StreamingPolicyStore.Get()
		d.Logger.Info("streaming compliance policy reloaded",
			"mode", string(policy.Mode),
			"failBehavior", string(policy.FailBehavior),
			"chunkBytes", policy.ChunkBytes,
			"hookTimeoutMs", policy.HookTimeoutMs,
		)
		return nil, nil
	})
}

type onboardingState struct {
	Enabled bool `json:"enabled"`
}

func registerOnboarding(l *cfgloader.Loader, d Deps) {
	cfgloader.Register(l, cfgloader.Handler[onboardingState]{
		Key:   "onboarding",
		Parse: cfgloader.ParseJSON[onboardingState](),
		Apply: func(ctx context.Context, v onboardingState, ver int64) ([]byte, error) {
			d.ProxyServer.SetOnboardingEnabled(v.Enabled)
			d.Logger.Info("onboarding mode updated", "enabled", v.Enabled)
			return nil, nil
		},
	})
}

type logLevelState struct {
	Level string `json:"level"`
}

func registerLogLevel(l *cfgloader.Loader, d Deps) {
	cfgloader.Register(l, cfgloader.Handler[logLevelState]{
		Key:   "log_level",
		Parse: cfgloader.ParseJSON[logLevelState](),
		Apply: func(ctx context.Context, v logLevelState, ver int64) ([]byte, error) {
			applied := logging.SetLevel(v.Level)
			d.Logger.Info("log level updated via shadow",
				"requested", v.Level,
				"applied", applied.String(),
			)
			return nil, nil
		},
	})
}

