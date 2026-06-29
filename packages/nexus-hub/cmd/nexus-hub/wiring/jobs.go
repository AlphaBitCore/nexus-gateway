package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	fleetmgr "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	defjobs_audit "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/audit"
	defjobs_drift "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/drift"
	defjobs_expiry "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/expiry"
	defjobs_health "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/health"
	defjobs_metrics "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/metrics"
	defjobs_quota "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/quota"
	defjobs_retention "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/retention"
	defjobs_rollup "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/rollup"
	defjobs_semanticcacheflush "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/semanticcacheflush"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/scheduler"
	jobstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	sharedops "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// InitConsumerManager wires the MQ consumer manager (traffic + admin-audit +
// exemption consumers). Returns nil when cfg.Consumers.Enabled is false or
// mqConsumer is nil.
func InitConsumerManager(
	cfg *config.HubConfig,
	pool *pgxpool.Pool,
	mqConsumer mq.Consumer,
	opsReg *sharedops.Registry,
	logger *slog.Logger,
) *consumer.Manager {
	if !cfg.Consumers.Enabled || mqConsumer == nil {
		return nil
	}

	var consumers []consumer.NamedConsumer

	// TrafficEventWriter — 3 event queues → traffic_event table
	tew := consumer.NewTrafficEventWriter(
		pool, mqConsumer,
		consumer.TrafficEventWriterConfig{
			BatchSize:      cfg.Consumers.BatchSize,
			FlushInterval:  cfg.Consumers.FlushInterval,
			DrainDutyCycle: cfg.Consumers.TrafficDrainDutyCycle,
		},
		logger, opsReg,
	)
	// If the MQ consumer can report the NEXUS_EVENTS stream fill level, let the
	// drain override its CPU-yield throttle to full-speed as the backlog nears
	// MaxBytes — bounded catch-up instead of a sustained-saturation wedge.
	if s, ok := mqConsumer.(interface {
		StreamFillFraction(context.Context, string) (float64, bool)
	}); ok {
		tew.WithBacklogSampler(func(ctx context.Context) (float64, bool) {
			return s.StreamFillFraction(ctx, "NEXUS_EVENTS")
		})
	}
	consumers = append(consumers, consumer.NamedConsumer{Name: "traffic-event-writer", Consumer: tew})

	// AdminAuditWriter — nexus.event.admin-audit → AdminAuditLog table
	aaw := consumer.NewAdminAuditWriter(
		pool, mqConsumer,
		consumer.AdminAuditWriterConfig{
			BatchSize:     cfg.Consumers.BatchSize,
			FlushInterval: cfg.Consumers.FlushInterval,
		},
		logger, opsReg,
	)
	consumers = append(consumers, consumer.NamedConsumer{Name: "admin-audit-writer", Consumer: aaw})

	// ExemptionConsumer — nexus.event.exemption (agent auto-uploaded TLS-bump
	// exemptions) → exemption_request table as PENDING rows. Admin reviews
	// at /compliance/exemptions; approve creates a compliance_exemption_grant
	// which Hub's catbagent loader pushes back to agent + compliance-proxy
	// via Cat B "exemptions" (Cert-Pin Auto-Exemption).
	ec := consumer.NewExemptionConsumer(
		pool, mqConsumer,
		consumer.ExemptionConsumerConfig{
			BatchSize:     cfg.Consumers.BatchSize,
			FlushInterval: cfg.Consumers.FlushInterval,
		},
		logger, opsReg,
	)
	consumers = append(consumers, consumer.NamedConsumer{Name: "exemption-consumer", Consumer: ec})

	return consumer.NewManager(consumers, logger, opsReg)
}

// InitScheduler creates, registers all jobs, syncs definitions, recovers stale
// runs, and starts the scheduler. Returns nil when cfg.Scheduler.Enabled is
// false. The caller must defer sched.Stop().
func InitScheduler(
	ctx context.Context,
	cfg *config.HubConfig,
	pool *pgxpool.Pool,
	redisClient redis.UniversalClient,
	mqConsumer mq.Consumer,
	mqProducer mq.Producer,
	st *store.Store,
	mgr *fleetmgr.Manager,
	spill spillstore.SpillStore,
	opsReg *sharedops.Registry,
	alertStore *alerting.Store,
	raiser *alerting.Raiser,
	siemBridge *siem.Bridge,
	logger *slog.Logger,
) (*scheduler.Scheduler, error) {
	if !cfg.Scheduler.Enabled {
		return nil, nil
	}

	jobStore := jobstore.New(pool)
	sched := scheduler.New(logger).
		WithJobStore(jobStore).
		WithReplicaID(cfg.Hub.ID).
		WithMetrics(prometheus.DefaultRegisterer)

	sched.Register(defjobs_drift.NewDriftDetector(st, mgr, redisClient, cfg.Scheduler.DriftCheckInterval, opsReg, logger))
	sched.Register(defjobs_expiry.NewOverrideExpiry(st, mgr, cfg.Scheduler.OverrideExpiryInterval, opsReg, logger))
	sched.Register(defjobs_audit.NewAuditChainVerify(pool, cfg.Scheduler.AuditChainVerifyInterval, opsReg, logger))

	// Audit pipeline freshness — defaults: tick every 60s, alarm at 5min stale.
	// Catches the silent-stall failure class (INSERT fails after consumer pull).
	sched.Register(defjobs_audit.NewAuditFreshnessCheck(pool, 60*time.Second, 5*time.Minute, opsReg, logger))

	sched.Register(defjobs_drift.NewIdentityEnricher(st, cfg.Scheduler.IdentityEnrichInterval, opsReg, logger))
	sched.Register(defjobs_expiry.NewAuthCleanup(st.AuthStore(), time.Hour, logger))
	sched.Register(defjobs_expiry.NewEnrollmentTokenCleanup(st, time.Hour, logger))
	// ServiceThreshold left at its default (90s = 3x the 30s ping interval) so a
	// single jittered/missed ping cannot flap a healthy service offline.
	sched.Register(defjobs_drift.NewStaleThingJob(st.RegistryStore(), 30*time.Second, logger, defjobs_drift.StaleThingConfig{}))
	sched.Register(defjobs_retention.NewJobRetention(jobStore, 24*time.Hour, 100, logger))
	sched.Register(defjobs_drift.NewExemptionGC(pool, mgr, cfg.Scheduler.Intervals.ExemptionGC, logger))

	sched.Register(defjobs_retention.NewDataRetention(pool, defjobs_retention.DataRetentionConfig{
		TrafficEventDays:        cfg.Scheduler.Retention.TrafficEventDays,
		TrafficEventPayloadDays: cfg.Scheduler.Retention.TrafficEventPayloadDays,
		AdminAuditLogDays:       cfg.Scheduler.Retention.AdminAuditLogDays,
	}, cfg.Scheduler.Intervals.DataRetention, logger))
	sched.Register(defjobs_rollup.NewRollupRetention(pool, defjobs_rollup.RollupRetentionConfig{
		Rollup5mDays:  cfg.Scheduler.Retention.Rollup5mDays,
		Rollup1hDays:  cfg.Scheduler.Retention.Rollup1hDays,
		Rollup1dDays:  cfg.Scheduler.Retention.Rollup1dDays,
		Rollup1moDays: cfg.Scheduler.Retention.Rollup1moDays,
	}, cfg.Scheduler.Intervals.RollupRetention, logger))
	sched.Register(defjobs_metrics.NewMetricsRollup(pool, cfg.Scheduler.Intervals.MetricsRollup, logger))

	// Smart-group membership recompute — every 60s as a safety net.
	sched.Register(defjobs_drift.NewSmartGroupRecompute(st.SmartGroupStore(), 60*time.Second, logger))

	rollup5m := defjobs_rollup.NewRollup5m(pool, cfg.Scheduler.Intervals.Rollup5m, logger, cfg.Scheduler.ExcludeInternalOpsFromBilledCost)
	merge1h := defjobs_rollup.NewRollupMerge1h(pool, cfg.Scheduler.Intervals.Merge1h, logger)
	merge1d := defjobs_rollup.NewRollupMerge1d(pool, cfg.Scheduler.Intervals.Merge1d, logger)
	merge1mo := defjobs_rollup.NewRollupMerge1mo(pool, cfg.Scheduler.Intervals.Merge1mo, logger)
	sched.Register(rollup5m)
	sched.Register(merge1h)
	sched.Register(merge1d)
	sched.Register(merge1mo)

	// Per-Thing rollup pipeline. Independent watermarks; EnableAgentRollup
	// gates whether source=agent rows are aggregated.
	thingRollup5m := defjobs_rollup.NewThingRollup5m(pool, cfg.Scheduler.Intervals.Rollup5m, logger, cfg.Scheduler.EnableAgentRollup, cfg.Scheduler.ExcludeInternalOpsFromBilledCost)
	thingMerge1h := defjobs_rollup.NewThingRollupMerge1h(pool, cfg.Scheduler.Intervals.Merge1h, logger)
	thingMerge1d := defjobs_rollup.NewThingRollupMerge1d(pool, cfg.Scheduler.Intervals.Merge1d, logger)
	thingMerge1mo := defjobs_rollup.NewThingRollupMerge1mo(pool, cfg.Scheduler.Intervals.Merge1mo, logger)
	sched.Register(thingRollup5m)
	sched.Register(thingMerge1h)
	sched.Register(thingMerge1d)
	sched.Register(thingMerge1mo)
	// lookbackDays = 0 → default (correctionLookbackDays) so late events up to
	// the agent offline-buffer horizon are still folded into the rollups.
	sched.Register(defjobs_rollup.NewRollupCorrection(rollup5m, merge1h, merge1d, merge1mo, 0, cfg.Scheduler.Intervals.RollupCorrection, logger))
	// Per-Thing correction sibling — without it late events whose per-Thing 5m
	// bucket already sealed are never re-aggregated.
	sched.Register(defjobs_rollup.NewThingRollupCorrection(thingRollup5m, thingMerge1h, thingMerge1d, thingMerge1mo, 0, cfg.Scheduler.Intervals.RollupCorrection, logger))

	sched.Register(defjobs_metrics.NewOpsRollup5m(pool, cfg.Scheduler.Intervals.OpsRollup5m, logger))
	sched.Register(defjobs_metrics.NewOpsRollup1h(pool, cfg.Scheduler.Intervals.OpsRollup1h, logger))
	sched.Register(defjobs_metrics.NewOpsRollup1d(pool, cfg.Scheduler.Intervals.OpsRollup1d, logger))
	sched.Register(defjobs_metrics.NewOpsRollup1mo(pool, cfg.Scheduler.Intervals.OpsRollup1mo, logger))
	sched.Register(defjobs_retention.NewOpsRetention(pool, cfg.Scheduler.Intervals.OpsRetention, logger))
	sched.Register(defjobs_retention.NewOpsRawPartition(pool, cfg.Scheduler.Intervals.OpsRawPartition, cfg.Scheduler.Retention.OpsRawDays, logger))

	sched.Register(defjobs_expiry.NewVKExpiry(pool, raiser, cfg.Scheduler.Intervals.VKExpiry, logger))
	sched.Register(defjobs_expiry.NewCredentialExpiry(pool, raiser, cfg.Scheduler.Intervals.CredentialExpiry, logger))
	sched.Register(defjobs_quota.NewQuotaAlertCheck(pool, raiser, cfg.Scheduler.Intervals.QuotaAlertCheck, logger))
	sched.Register(defjobs_health.NewThingOfflineAlerts(pool, raiser, alertStore, cfg.Scheduler.Intervals.ThingOfflineAlerts, logger))
	sched.Register(defjobs_health.NewProviderUnavailableAlerts(pool, raiser, alertStore, cfg.Scheduler.Intervals.ProviderUnavailableAlerts, logger))

	// State-poll alert jobs (class 1, not Engine).
	sched.Register(defjobs_health.NewAgentCertExpirationAlerts(pool, raiser, alertStore, cfg.Scheduler.Intervals.AgentCertExpiry, logger))
	sched.Register(defjobs_health.NewCredentialStaleAlerts(pool, raiser, alertStore, cfg.Scheduler.Intervals.CredentialStale, logger))
	sched.Register(defjobs_retention.NewCredentialStatsFlush(pool, redisClient, cfg.Scheduler.Intervals.CredentialStatsFlush, logger))

	// Drain cred:circuit:dirty into Credential.circuit* columns.
	credCircuitFlushMetrics := defjobs_retention.NewCircuitFlushMetrics(prometheus.DefaultRegisterer)
	sched.Register(defjobs_retention.NewCredentialCircuitFlush(pool, redisClient, cfg.Hub.ID,
		cfg.Scheduler.Intervals.CredentialCircuitFlush, logger, credCircuitFlushMetrics))

	// Per-credential health rollup (5min + 1h windows).
	credReliabilityLoader := &defjobs_rollup.ReliabilityThresholdsLoader{Pool: pool, Logger: logger}
	credHealthRollupMetrics := defjobs_rollup.NewHealthRollupMetrics(prometheus.DefaultRegisterer)
	sched.Register(defjobs_rollup.NewCredentialHealthRollup(pool, credReliabilityLoader,
		cfg.Scheduler.Intervals.CredentialHealthRollup, logger, credHealthRollupMetrics))

	// Reliability alerts — circuit open, health unavailable, sustained degraded ≥15min.
	sched.Register(defjobs_health.NewCredentialReliabilityAlerts(pool, raiser, alertStore, credReliabilityLoader,
		cfg.Scheduler.Intervals.CredentialReliabilityAlerts, logger))

	sched.Register(defjobs_expiry.NewCredentialRetire(pool, cfg.Scheduler.Intervals.CredentialRetire, logger))
	sched.Register(defjobs_health.NewCacheQualityMonitor(pool, cfg.Scheduler.Intervals.CacheQualityMonitor, logger))

	// Emergency-passthrough expiry auto-revert.
	sched.Register(defjobs_expiry.NewPassthroughExpiryJob(pool, 60*time.Second, logger))
	sched.Register(defjobs_rollup.NewProviderHealthRollup(pool, cfg.Scheduler.Intervals.ProviderHealthRollup, logger))

	// Alerteval streaming engine — class-4 event-stream rules.
	registerAlertEvalEngine(cfg, pool, mqConsumer, raiser, alertStore, sched, logger)

	// SIEM bridge scheduler job.
	if siemBridge != nil {
		sched.Register(defjobs_audit.NewSIEMBridge(siemBridge, logger))
	}

	// Semantic cache reindex — blue/green Valkey vector index swap when
	// the embedding model fingerprint changes. Runs every 5s; no-ops when
	// fingerprints already match. redisClient may be nil (job no-ops safely).
	sched.Register(defjobs_semanticcacheflush.New(pool, redisClient, cfg.Scheduler.Intervals.SemanticCacheReindex, logger))

	if err := sched.SyncDefinitions(ctx); err != nil {
		return nil, err
	}
	if err := sched.RecoverStaleRuns(ctx); err != nil {
		// Non-fatal: log and continue; stale rows are cosmetic, not blocking.
		logger.Warn("scheduler stale-run recovery failed", "error", err)
	}
	sched.Start()

	return sched, nil
}
