// Package compliance wires the shared hook pipeline into the Desktop Agent.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
)

// AgentPipeline holds the agent's compliance pipeline state. The hook
// resolver is owned by pipeline.HookConfigCache (from
// packages/shared/policy/pipeline) running with TTL=0 so reloads happen
// only on explicit ApplyHooksShadowState calls; ai-gateway and
// compliance-proxy use the same cache type. The traffic.DomainSnapshot
// tracks interception-domain state and stays a separate atomic pointer
// (it is unrelated to hooks).
type AgentPipeline struct {
	hookCache *pipeline.HookConfigCache
	// pendingConfigs bridges the agent's push semantics
	// (shadow.ApplyHooksShadowState) into HookConfigCache's
	// loader-based contract. Each Apply* call stores the new slice
	// here, then invokes hookCache.Reload, which pulls from this slot.
	pendingConfigs atomic.Pointer[[]core.HookConfig]

	// rulePacksByHookID maps boundHookId → installs to inject. The
	// installed_rule_packs Cat B applier populates this; Apply*Hooks
	// reads it to inject `_rulePackInstalls` into each hook's Config
	// so shared/hooks/keyword_filter.go:42 routes the factory to
	// NewRulePackEngine. Without this the rule packs reach the agent
	// (and show up in the Policies UI) but the actual scan rules
	// never fire. Empty map = no rule packs registered yet.
	rulePacksByHookID atomic.Pointer[map[string][]rulePackInstallView]

	snapshot     atomic.Pointer[traffic.DomainSnapshot]
	registry     *traffic.AdapterRegistry
	hookRegistry *core.HookRegistry
	logger       *slog.Logger

	// domainEngine carries the agent-side priority-ordered host matcher
	// built from the interception_domains shadow snapshot.
	// shared/tlsbump.WithDomainEngine consumes it during MITM-bumped
	// requests for adapter resolution + per-host PROCESS/PASSTHROUGH/
	// BLOCK decisions. Eager-initialised in the ctor to a non-nil empty
	// engine so boot-time callers see a valid pointer; populated by
	// ApplyDomainsShadowState on each interception_domains push.
	domainEngine *domain.Engine
}

// rulePackInstallView is the runtime projection of one installed rule
// pack consumed by NewRulePackEngine. Field names match the JSON the
// shared/hooks/rulepack_engine factory expects to find at
// cfg.Config["_rulePackInstalls"]. Local copy (not imported from
// shared/hooks) to avoid a circular dep — the marshalled JSON is the
// contract, not the Go type.
type rulePackInstallView struct {
	InstallID   string             `json:"installId"`
	PackName    string             `json:"packName"`
	PackVersion string             `json:"packVersion"`
	Enabled     bool               `json:"enabled"`
	Rules       []rulePackRuleView `json:"rules"`
}

type rulePackRuleView struct {
	RuleID      string   `json:"ruleId"`
	Category    string   `json:"category"`
	Severity    string   `json:"severity"`
	Pattern     string   `json:"pattern"`
	Flags       string   `json:"flags,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// NewAgentPipeline creates the agent's compliance pipeline.
func NewAgentPipeline(logger *slog.Logger) *AgentPipeline {
	return newAgentPipelineWithRegistry(logger, builtins.Registry)
}

// newAgentPipelineWithRegistry is the internal constructor that allows tests
// to inject a custom hook registry. Production code calls NewAgentPipeline,
// which always passes the package-global hooks.Registry.
func newAgentPipelineWithRegistry(logger *slog.Logger, registry *core.HookRegistry) *AgentPipeline {
	reg := traffic.NewAdapterRegistry("nexus_agent")
	adapters.RegisterBuiltins(reg)
	reg.Freeze()

	p := &AgentPipeline{
		registry:     reg,
		hookRegistry: registry,
		logger:       logger,
		// Eager-init the host-match engine so callers that read
		// DomainEngine() at boot (platformshim/wire_bridge_darwin.go,
		// wiring/bridge.go) see a non-nil pointer instead of waiting for
		// the first interception_domains shadow push. The empty engine
		// fail-opens (MatchHost returns nil). The bridge wiring requires
		// a non-nil pointer at construct time.
		domainEngine: domain.NewEngine(),
	}
	p.snapshot.Store(traffic.Empty())

	// HookConfigCache loader returns whatever pendingConfigs holds.
	// TTL=0 disables the auto-reload backstop in HookConfigCache, so
	// the resolver is only ever swapped when an Apply*Shadow call
	// stores a new slice and explicitly invokes Reload.
	p.hookCache = pipeline.NewHookConfigCache(
		func(_ context.Context) ([]core.HookConfig, error) {
			if cfgs := p.pendingConfigs.Load(); cfgs != nil {
				return *cfgs, nil
			}
			return nil, nil
		},
		registry,
		0,
		logger,
	)
	// Trigger an empty initial load so Resolver() never returns nil.
	if err := p.hookCache.Reload(context.Background()); err != nil {
		logger.Warn("agent compliance pipeline: initial empty resolver load failed", "error", err)
	}
	return p
}

// Resolver returns the current PolicyResolver (safe for concurrent use).
func (p *AgentPipeline) Resolver() *pipeline.PolicyResolver {
	return p.hookCache.Resolver(context.Background())
}

// EvaluateConnectionInput carries the subset of HookInput relevant to the
// connection-stage pipeline. Populated by the platform shim after SNI
// resolution, before MITM relay. RequestID is optional; the agent does not
// assign one at connection stage today.
type EvaluateConnectionInput struct {
	RequestID  string
	SourceIP   string
	TargetHost string
	SNI        string
	// ClientCertFingerprint is populated when the agent's TLS bridge sees a
	// client cert (rare in transparent interception but reserved).
	ClientCertFingerprint string
}

// connectionStageTimeouts mirror the request-stage defaults used by the
// intercept handler. Connection-stage hooks are expected to be cheap
// policy checks (host blocklist, ip-access), so a 5s per-hook / 30s total
// ceiling is generous.
const (
	connectionStagePerHookTimeout = 5 * time.Second
	connectionStageTotalTimeout   = 30 * time.Second
)

// EvaluateConnection runs the connection-stage hook pipeline and reports
// whether the connection should be rejected. On any infrastructure error
// (resolver / pipeline build / timeout) the call returns blocked=false
// with an empty reason — fail-open, matching ai-gateway and compliance-proxy.
// Callers are expected to close the TCP connection (or return DecisionDeny)
// on blocked=true; agent has no HTTP-layer response to send at this stage.
func (p *AgentPipeline) EvaluateConnection(ctx context.Context, in EvaluateConnectionInput) (blocked bool, reason string) {
	resolver := p.hookCache.Resolver(ctx)
	if resolver == nil {
		return false, ""
	}
	// Connection stage has no endpoint type; pass "" and nil modalities
	// to preserve fail-open behavior.
	pipe, err := resolver.BuildPipeline(
		"connection",
		"AGENT",
		"", nil,
		connectionStagePerHookTimeout,
		connectionStageTotalTimeout,
		false,
		p.logger,
	)
	if err != nil {
		p.logger.Warn("agent connection-stage pipeline build error; failing open", "error", err)
		return false, ""
	}
	if pipe == nil {
		// No connection-stage hooks configured — fast path.
		return false, ""
	}
	input := &core.HookInput{
		RequestID:   in.RequestID,
		Stage:       "connection",
		SourceIP:    in.SourceIP,
		TargetHost:  in.TargetHost,
		Method:      "CONNECT",
		IngressType: "AGENT",
		TLS: &core.TLSInfo{
			SNI:                   in.SNI,
			ClientCertFingerprint: in.ClientCertFingerprint,
		},
	}
	res := pipe.Execute(ctx, input)
	if res != nil && res.Decision == core.RejectHard {
		r := res.Reason
		if r == "" {
			r = "connection blocked by compliance policy"
		}
		return true, r
	}
	return false, ""
}

// Snapshot returns the current DomainSnapshot (safe for concurrent use).
func (p *AgentPipeline) Snapshot() *traffic.DomainSnapshot {
	return p.snapshot.Load()
}

// ApplySnapshot atomically replaces both the PolicyResolver and the
// DomainSnapshot with new instances built from the config snapshot.
// Creating fresh objects (instead of mutating the existing ones) ensures
// that the resolver and snapshot are always consistent as a unit — a reader
// that loaded the previous pair sees no partial update.
func (p *AgentPipeline) ApplySnapshot(snap *shadow.ConfigSnapshot) {
	// Stage the new hook configs and trigger HookConfigCache to swap the
	// resolver atomically (same atomic-pointer path the other two data
	// planes use).
	cfgs := snap.HookConfigs
	p.pendingConfigs.Store(&cfgs)
	if err := p.hookCache.Reload(context.Background()); err != nil {
		p.logger.Warn("agent compliance pipeline: hook reload failed", "error", err)
	} else {
		p.logger.Info("policy resolver replaced",
			slog.Int("hookConfigs", len(cfgs)),
		)
	}

	// Build and swap the DomainSnapshot for traffic interception.
	domains, paths := convertDomainDTOs(snap.InterceptionDomains)
	domSnap := traffic.BuildDomainSnapshot(domains, paths, p.registry, p.logger)
	p.snapshot.Store(domSnap)
	p.logger.Info("domain snapshot replaced",
		slog.Int("domains", domSnap.Size()),
	)
}

// ApplyHooksShadowState implements shadow.ShadowApplier for the hooks key.
// raw carries the aggregated hook list fetched by the Manager from Hub via
// Cat B pull; empty / null / "{}" is a no-op so an early tick before Cat B
// aggregation lands does not blank the resolver. An explicit
// {"hookConfigs": []} IS authoritative and replaces the resolver with an
// empty one.
func (p *AgentPipeline) ApplyHooksShadowState(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return nil
	}
	var payload struct {
		HookConfigs []core.HookConfig `json:"hookConfigs"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("pipeline: parse hooks shadow state: %w", err)
	}
	cfgs := injectRulePacks(payload.HookConfigs, p.rulePacksByHookID.Load(), p.logger)
	p.pendingConfigs.Store(&cfgs)
	if err := p.hookCache.Reload(ctx); err != nil {
		return fmt.Errorf("pipeline: hook cache reload: %w", err)
	}
	p.logger.Info("policy resolver replaced (shadow)",
		slog.Int("hookConfigs", len(cfgs)),
	)
	return nil
}

// ApplyRulePacksShadowState implements shadow.ShadowApplier for the
// installed_rule_packs key. raw carries the {"installedRulePacks":[...]}
// envelope packages/nexus-hub/internal/storage/store/catb_agent_installed_rule_packs.go
// emits. Indexes the packs by boundHookId so ApplyHooksShadowState can
// inject `_rulePackInstalls` into each matching HookConfig.Config map
// at hook reload time, which routes the keyword_filter factory to
// NewRulePackEngine. After indexing we re-run hook reload so a pack
// that arrives AFTER the hooks have already loaded still takes effect
// without waiting for the next hooks push.
func (p *AgentPipeline) ApplyRulePacksShadowState(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		// Empty payload = no installed packs; clear the registry so
		// existing hooks lose any previously-injected packs on the
		// next hook reload.
		empty := map[string][]rulePackInstallView{}
		p.rulePacksByHookID.Store(&empty)
		return p.reloadHooksWithCurrentPacks(ctx)
	}
	var payload struct {
		InstalledRulePacks []struct {
			ID          string             `json:"id"`
			PackID      string             `json:"packId"`
			Name        string             `json:"name"`
			Version     string             `json:"version"`
			BoundHookID string             `json:"boundHookId"`
			Enabled     bool               `json:"enabled"`
			Rules       []rulePackRuleView `json:"rules"`
		} `json:"installedRulePacks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("pipeline: parse rule_packs shadow state: %w", err)
	}
	idx := map[string][]rulePackInstallView{}
	totalRules := 0
	for _, pk := range payload.InstalledRulePacks {
		if pk.BoundHookID == "" {
			// Pack not bound to any hook → nothing to inject. The
			// Policies UI still shows it (wire payload includes it);
			// just no enforcement until admin binds it.
			continue
		}
		install := rulePackInstallView{
			InstallID:   pk.ID,
			PackName:    pk.Name,
			PackVersion: pk.Version,
			Enabled:     pk.Enabled,
			Rules:       pk.Rules,
		}
		idx[pk.BoundHookID] = append(idx[pk.BoundHookID], install)
		totalRules += len(pk.Rules)
	}
	p.rulePacksByHookID.Store(&idx)
	p.logger.Info("rule pack registry updated",
		slog.Int("packs", len(payload.InstalledRulePacks)),
		slog.Int("hooksBound", len(idx)),
		slog.Int("totalRules", totalRules),
	)
	return p.reloadHooksWithCurrentPacks(ctx)
}

// reloadHooksWithCurrentPacks re-runs hook reload using the in-memory
// pendingConfigs but with the latest rule pack registry injected. Used
// when installed_rule_packs arrives out-of-order with hooks so
// the new packs apply without waiting for the next hooks tick.
func (p *AgentPipeline) reloadHooksWithCurrentPacks(ctx context.Context) error {
	current := p.pendingConfigs.Load()
	if current == nil {
		// hooks hasn't landed yet — nothing to reload. The next
		// ApplyHooksShadowState call will pick up the registry.
		return nil
	}
	reInjected := injectRulePacks(*current, p.rulePacksByHookID.Load(), p.logger)
	p.pendingConfigs.Store(&reInjected)
	if err := p.hookCache.Reload(ctx); err != nil {
		return fmt.Errorf("pipeline: hook cache re-reload after rule pack update: %w", err)
	}
	return nil
}

// injectRulePacks walks the slice of hook configs and, for each hook
// whose ID matches the rule pack registry, sets cfg.Config["_rulePackInstalls"]
// to the list of installs. Always returns a copy of cfgs so the caller's
// input isn't mutated (we'd otherwise corrupt the original JSON-decoded
// slice and leak rule pack data into hook-config audit logging).
// nil registry / empty registry = identity copy.
func injectRulePacks(cfgs []core.HookConfig, registry *map[string][]rulePackInstallView, logger *slog.Logger) []core.HookConfig {
	if len(cfgs) == 0 {
		return cfgs
	}
	out := make([]core.HookConfig, len(cfgs))
	copy(out, cfgs)
	if registry == nil || len(*registry) == 0 {
		return out
	}
	for i := range out {
		installs, ok := (*registry)[out[i].ID]
		if !ok || len(installs) == 0 {
			continue
		}
		// Clone the config map so we don't mutate the original.
		newCfg := make(map[string]any, len(out[i].Config)+1)
		for k, v := range out[i].Config {
			newCfg[k] = v
		}
		// shared/hooks.parseRulePackInstalls accepts only its private
		// []rulePackInstall type or the generic []any shape (which it
		// JSON-roundtrips). Box each rulePackInstallView into an
		// []any so the field-name-equivalent JSON survives the
		// shared-side decode. Without this the rulepack-engine factory
		// rejects the type → keyword-blocker hook fails to build →
		// the entire request pipeline returns an error and the agent
		// drops every inspect-mode flow with no audit row.
		anyInstalls := make([]any, len(installs))
		for j := range installs {
			anyInstalls[j] = installs[j]
		}
		newCfg["_rulePackInstalls"] = anyInstalls
		out[i].Config = newCfg
		if logger != nil {
			logger.Debug("injected rule pack installs into hook config",
				"hookId", out[i].ID,
				"installs", len(installs),
			)
		}
	}
	return out
}

// ApplyDomainsShadowState implements shadow.ShadowApplier for the
// interception_domains key. raw carries the domain snapshot fetched by the
// Manager from Hub via Cat B pull; empty / null / "{}" is a no-op.
func (p *AgentPipeline) ApplyDomainsShadowState(_ context.Context, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return nil
	}
	var payload struct {
		InterceptionDomains []shadow.InterceptionDomainDTO `json:"interceptionDomains"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("pipeline: parse domains shadow state: %w", err)
	}
	domains, paths := convertDomainDTOs(payload.InterceptionDomains)
	domSnap := traffic.BuildDomainSnapshot(domains, paths, p.registry, p.logger)
	p.snapshot.Store(domSnap)

	// Feed the same domain list into a domain.Engine for shared/tlsbump
	// consumption. We use the configsync converter so the agent and cp
	// engines see structurally identical rows. Per-host streaming/capture
	// override columns that are absent on the agent's DTO land as nil
	// pointers; the resolver falls back to global StreamingPolicy /
	// payloadcapture defaults. Engine.Swap is idempotent + bad-config-safe
	// — a parse error on any row leaves the previous engine in place.
	dpDomains := (&shadow.ConfigSnapshot{InterceptionDomains: payload.InterceptionDomains}).ToDomainPolicy()
	if p.domainEngine == nil {
		p.domainEngine = domain.NewEngine()
	}
	if err := p.domainEngine.Swap(dpDomains); err != nil {
		// Don't propagate — domainpolicy errors must not knock out the
		// rest of the shadow apply. The previous engine snapshot stays.
		p.logger.Warn("domain.Engine swap rejected; keeping previous snapshot", "error", err)
	}

	p.logger.Info("domain snapshot replaced (shadow)",
		slog.Int("domains", domSnap.Size()),
	)
	return nil
}

// AdapterRegistry returns the agent's traffic-adapter registry for use by
// shared/tlsbump for per-domain DetectRequestMeta / DetectResponseUsage /
// ExtractResponse.
func (p *AgentPipeline) AdapterRegistry() *traffic.AdapterRegistry {
	return p.registry
}

// DomainEngine returns the live domain.Engine populated by
// ApplyDomainsShadowState. wiring/bridge.go reads this on bridge-deps
// construction so the agent honours the same per-host adapter +
// PROCESS/PASSTHROUGH/BLOCK semantics that shared/tlsbump expects. The
// engine is eager-initialised at construction and fail-opens until the
// first interception_domains push arrives.
func (p *AgentPipeline) DomainEngine() *domain.Engine {
	return p.domainEngine
}

// convertDomainDTOs converts the wire-format DTOs to configtypes used by
// traffic.BuildDomainSnapshot.
func convertDomainDTOs(dtos []shadow.InterceptionDomainDTO) ([]interception.InterceptionDomain, []interception.InterceptionPath) {
	var domains []interception.InterceptionDomain
	var allPaths []interception.InterceptionPath

	now := time.Now()
	for _, d := range dtos {
		domains = append(domains, interception.InterceptionDomain{
			Id:                d.ID,
			Name:              d.Name,
			HostPattern:       d.HostPattern,
			HostMatchType:     interception.HostMatchType(d.HostMatchType),
			AdapterId:         d.AdapterID,
			AdapterConfig:     d.AdapterConfig,
			Enabled:           d.Enabled,
			Priority:          int32(d.Priority),
			DefaultPathAction: interception.DefaultPathAction(d.DefaultPathAction),
			OnAdapterError:    interception.FailureAction(d.OnAdapterError),
			NetworkZone:       interception.NetworkZone(d.NetworkZone),
			CreatedAt:         now,
			UpdatedAt:         now,
		})

		for _, path := range d.Paths {
			allPaths = append(allPaths, interception.InterceptionPath{
				Id:          path.ID,
				DomainId:    d.ID,
				PathPattern: path.PathPattern,
				MatchType:   interception.PathMatchType(path.MatchType),
				Action:      interception.PathAction(path.Action),
				Priority:    int32(path.Priority),
				Enabled:     path.Enabled,
				CreatedAt:   now,
				UpdatedAt:   now,
			})
		}
	}

	return domains, allPaths
}
