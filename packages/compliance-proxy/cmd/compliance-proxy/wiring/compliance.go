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
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillsweep"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// ComplianceResult holds the components created by InitCompliance.
type ComplianceResult struct {
	Resolver        *compliance.PolicyResolver
	HookConfigCache *pipeline.HookConfigCache
	Emitter         *compliance.AuditEmitter
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
}

// InitCompliance initializes the compliance kernel: config cache loading,
// hook configs from the database, policy resolver, and extractor registry.
func InitCompliance(cfg *config.Config, cacheManager *cache.Manager, auditWriter audit.Writer, logger *slog.Logger) (ComplianceResult, error) {
	var result ComplianceResult

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
		builtins.Registry,
		2*time.Minute,
		logger,
	)
	// Redis and Start are handled after context creation in main.go.
	// Perform initial load synchronously here.
	initCtx, initCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := result.HookConfigCache.Reload(initCtx); err != nil {
		initCancel()
		return result, fmt.Errorf("load initial hook configs from database: %w", err)
	}
	initCancel()

	result.Resolver = result.HookConfigCache.Resolver(context.Background())

	if auditWriter != nil {
		// WithPreSpillNormalize: the proxy's audit writer runs a
		// flush-time normalize pass (applyNormalize), so retaining a
		// spilled body's in-memory bytes lets it project spill-destined
		// traffic without a spill-store fetch. The agent does NOT set this
		// — it normalizes inline before emit.
		result.Emitter = compliance.NewAuditEmitter(auditWriter, logger).WithPreSpillNormalize()
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
