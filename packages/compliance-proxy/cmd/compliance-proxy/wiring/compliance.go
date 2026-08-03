package wiring

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/loaders"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
	hookwh "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/webhook"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/thingtype"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillsweep"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/peerurl"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// ComplianceResult holds the components created by InitCompliance.
type ComplianceResult struct {
	Resolver        *compliance.PolicyResolver
	HookConfigCache *pipeline.HookConfigCache
	Emitter         *compliance.AuditEmitter
	// SpillAvailability records whether an oversize captured body actually has
	// anywhere to go on this node. Surfaced as the storage.spill runtime source
	// so a configured inline-vs-spill threshold cannot silently mean nothing.
	SpillAvailability spillfactory.Availability
	// MatcherEngine records which content-scanning engine THIS BINARY compiled
	// in. It is a build-tag choice with an order-of-magnitude latency
	// consequence on large bodies, and it was previously answerable only by
	// inspecting the build — a cross-compiled binary that lost its cgo engine
	// looked identical at runtime.
	MatcherEngine matcher.Engine
	// SpillStore + SpillConfig are carried so the storage.spill introspection
	// source can measure RESIDENCY when it is read, not once at boot: "how much is
	// spilled" is a question whose answer changes every minute, and a boot-time
	// snapshot would answer it with the number from process start.
	SpillStore  spillstore.SpillStore
	SpillConfig spillfactory.FactoryConfig
	// StreamingMode field removed — admin policy is the
	// single source of truth; downstream reads from
	// *streampolicy.Store directly.
	LiveConfig   streaming.LiveConfig
	PerHookTmout time.Duration
	TotalTmout   time.Duration
	Parallel     bool
	ConfigDB     *sql.DB
	// RulePackPool is a pgx pool used exclusively by the rule-pack store
	// (shared/rulepack.Store expects pgxpool, not database/sql.DB). It is
	// shared by rulepack.Enrich calls during hook-config reloads.
	RulePackPool *pgxpool.Pool
	// PeerResolver is the process-wide Hub-backed resolver for peer service
	// base URLs (lazy, cached). Consumers: the webhook-forward trusted
	// AI-Guard bases (this file) and the onboarding 407 CP-UI link default
	// (listener.go). One resolver per process — do not construct more.
	// Always non-nil, even when the compliance kernel is disabled.
	PeerResolver *peerurl.Resolver
}

// InitCompliance initializes the compliance kernel: config cache loading,
// hook configs from the database, policy resolver, and extractor registry.
func InitCompliance(cfg *config.Config, cacheManager *cache.Manager, auditWriter audit.Writer, logger *slog.Logger) (ComplianceResult, error) {
	var result ComplianceResult

	// The peer resolver is process-wide state, not compliance-kernel state:
	// the onboarding 407 CP-UI link default needs it even when compliance is
	// disabled, so it is built before the enabled gate. Construction does no
	// I/O — resolution is lazy at first use.
	result.PeerResolver = peerurl.New(cfg.Registry.NexusHubURL, cfg.Auth.InternalServiceToken)

	if !cfg.Compliance.Enabled {
		slog.Info("compliance kernel disabled")
		return result, nil
	}

	// Hook configs are loaded from the Prisma-migrated database; a URL is required.
	if cfg.Database.URL == "" {
		return result, fmt.Errorf("compliance is enabled but database.url is empty — hook configs are loaded from the Prisma-migrated database, a URL is required (set via env DATABASE_URL)")
	}

	var err error
	result.ConfigDB, err = sql.Open("pgx", cfg.Database.URL)
	if err != nil {
		return result, fmt.Errorf("configcache: open database: %w", err)
	}
	// Keep this pool small — it only serves occasional config reloads.
	result.ConfigDB.SetMaxOpenConns(4)
	result.ConfigDB.SetMaxIdleConns(2)
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := result.ConfigDB.PingContext(pingCtx); err != nil {
		pingCancel()
		return result, fmt.Errorf("configcache: ping database: %w", err)
	}
	pingCancel()

	// YAML hooks are ignored — the database is the single source of truth.
	if len(cfg.Compliance.Hooks) > 0 {
		slog.Warn("compliance.hooks in compliance-proxy.config.yaml is ignored; hook configs are loaded from the database. Remove the YAML entries to silence this warning.",
			"ignoredCount", len(cfg.Compliance.Hooks),
		)
	}

	// Separate pgxpool for rule-pack reads. shared/rulepack.Store only
	// supports pgxpool, and we do not want to force ConfigDB (database/sql)
	// consumers onto pgxpool just for this side path. Small pool — one
	// concurrent loader per reload cycle is plenty.
	rulePackPool, err := pgxpool.New(context.Background(), cfg.Database.URL)
	if err != nil {
		return result, fmt.Errorf("rulepack: open pgx pool: %w", err)
	}
	result.RulePackPool = rulePackPool
	rulePackStore := rulepack.NewStore(rulePackPool)

	// Clone the shared registry and replace webhook-forward with a variant
	// that authenticates to this deployment's AI-Guard compliance-webhook. The
	// trusted AI-Gateway bases are RESOLVED from the Hub (the ai-gateway
	// Thing's reported publicUrl + privateUrl) — never configured locally, so
	// they cannot drift. The token rides only when a hook endpoint's
	// scheme+host match a resolved base AND its path is the
	// compliance-webhook, so the [MUST MATCH] internal secret never leaks to
	// an arbitrary webhook URL. Resolution is lazy + cached (peerurl); before
	// the ai-gateway has reported, the provider returns nothing and the hook
	// posts unauthenticated (fail-safe), retrying next request. The nil
	// client keeps the per-hook SSRF-guarded egress the builtin registered.
	peerResolver := result.PeerResolver
	trustedAIGuardBases := func(ctx context.Context) []string {
		// One resolution covers both kinds — URLs() hits the cache/Hub once
		// per call instead of twice.
		pub, priv, err := peerResolver.URLs(ctx, thingtype.AIGateway)
		if err != nil {
			return nil
		}
		bases := make([]string, 0, 2)
		if pub != "" {
			bases = append(bases, pub)
		}
		if priv != "" {
			bases = append(bases, priv)
		}
		return bases
	}
	hookRegistry := builtins.Registry.Clone()
	hookRegistry.Replace("webhook-forward", func(hcfg *core.HookConfig) (core.Hook, error) {
		return hookwh.NewWebhookForwardWithOptions(hcfg, hookwh.Options{
			InternalToken:       cfg.Auth.InternalServiceToken,
			TrustedAIGuardBases: trustedAIGuardBases,
		})
	})
	hookRegistry.Freeze()

	// Use the shared HookConfigCache for hook config management.
	configDB := result.ConfigDB
	result.HookConfigCache = pipeline.NewHookConfigCache(
		func(ctx context.Context) ([]core.HookConfig, error) {
			cfgs, err := loaders.LoadHookConfigs(ctx, configDB)
			if err != nil {
				return nil, err
			}
			if cfgs, err = rulepack.Enrich(ctx, rulePackStore, cfgs); err != nil {
				return nil, fmt.Errorf("hooks: enrich rule packs: %w", err)
			}
			return cfgs, nil
		},
		hookRegistry,
		2*time.Minute,
		logger,
	)
	// Initial load happens synchronously here and FAILS the boot on error —
	// the compliance plane must never start serving with an empty hook
	// config. main.go arms the TTL-backstop ticker via HookConfigCache.Start
	// once the signal context exists; Hub push stays the primary update path.
	initCtx, initCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := result.HookConfigCache.Reload(initCtx); err != nil {
		initCancel()
		return result, fmt.Errorf("load initial hook configs from database: %w", err)
	}
	initCancel()

	result.Resolver = result.HookConfigCache.Resolver(context.Background())

	// Describe the spill posture unconditionally. It used to sit inside the
	// audit branch below, so a node with audit disabled served an empty `effect`
	// from the storage.spill runtime source — no explanation at all, even with a
	// real backend configured. Whether audit is on does not change where an
	// oversize body would go, and the gateway already describes unconditionally.
	result.SpillAvailability = spillfactory.Describe(cfg.Spill, nil)
	result.SpillConfig = cfg.Spill

	// Say which scanning engine is in force, once, at boot. Same reasoning as the
	// spill posture above: a setting whose effect is invisible cannot be verified
	// by the people responsible for it.
	result.MatcherEngine = matcher.DescribeEngine()
	logger.Info("rule-pack scanning engine",
		"engine", result.MatcherEngine.Name,
		"singlePass", result.MatcherEngine.SinglePass,
		"effect", result.MatcherEngine.Effect,
	)

	if auditWriter != nil {
		result.Emitter = compliance.NewAuditEmitter(auditWriter, logger)
		// Wire spillstore for out-of-band body storage. Returns
		// (nil, nil) when cfg.Spill.Enabled is false; emitter then keeps
		// inline-only behaviour. The runtime MaxInlineBodyBytes
		// threshold is wired into the emitter from main.go via
		// WithPayloadCaptureStore once the payload-capture store has
		// been seeded from system_metadata.
		spillStore, err := spillfactory.New(cfg.Spill, logger)
		if err != nil {
			return result, fmt.Errorf("spillstore init: %w", err)
		}
		// Re-describe now that the store is built: the nil case is the one an
		// operator cannot otherwise see, because a configured threshold then
		// does nothing at all.
		result.SpillAvailability = spillfactory.Describe(cfg.Spill, spillStore)
		result.SpillStore = spillStore
		if spillStore != nil {
			result.Emitter.WithSpillStore(spillStore)
			// Process-lifetime sweep so the backend's retention horizon and
			// total-size cap are enforced (the store is per-process).
			go spillsweep.Run(context.Background(), spillStore, spillsweep.Options{
				Retention: cfg.Spill.RetentionHorizon(),
			}, logger)
		}
	}

	perHookMs := cfg.Compliance.PerHookTimeoutMs
	if perHookMs <= 0 {
		perHookMs = 5000
	}
	totalMs := cfg.Compliance.TotalTimeoutMs
	if totalMs <= 0 {
		totalMs = 15000
	}
	result.PerHookTmout = time.Duration(perHookMs) * time.Millisecond
	result.TotalTmout = time.Duration(totalMs) * time.Millisecond
	result.Parallel = cfg.Compliance.ParallelHooks

	checkpointChars := cfg.Compliance.CheckpointChars
	if checkpointChars <= 0 {
		checkpointChars = 500
	}
	result.LiveConfig = streaming.LiveConfig{
		CheckpointChars: checkpointChars,
	}

	slog.Info("compliance kernel initialized",
		"source", "database via HookConfigCache",
		"parallelHooks", result.Parallel,
		"perHookTimeoutMs", perHookMs,
		"totalTimeoutMs", totalMs,
	)

	return result, nil
}
