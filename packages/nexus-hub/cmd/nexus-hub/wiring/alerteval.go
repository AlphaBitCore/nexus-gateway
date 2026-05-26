package wiring

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	alerteval "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval/aggregators"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/scheduler"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// registerAlertEvalEngine wires the alerteval streaming engine onto the
// scheduler. No-ops (logs a warning) when mqConsumer is nil.
// The engine subscribes to MQ traffic + audit events under consumer group
// "hub-alerting", maintains in-memory ring buffers per Aggregator, and ticks
// every cfg.Scheduler.AlertEval.EngineTickSec seconds (default 5).
func registerAlertEvalEngine(
	cfg *config.HubConfig,
	pool *pgxpool.Pool,
	mqConsumer mq.Consumer,
	raiser *alerting.Raiser,
	alertStore *alerting.Store,
	sched *scheduler.Scheduler,
	logger *slog.Logger,
) {
	if mqConsumer == nil {
		logger.Warn("alerteval engine NOT registered: MQ consumer unavailable")
		return
	}

	eng := alerteval.NewEngine(
		alerteval.Config{
			TickSec:   cfg.Scheduler.AlertEval.EngineTickSec,
			StartTime: time.Now().UTC(),
		},
		pool, mqConsumer, raiser, alertStore, logger,
	)
	eng.Register(aggregators.NewHookRejectRate())
	eng.Register(aggregators.NewVKTrafficSpike())
	eng.Register(aggregators.NewLoginFailureFlood())
	eng.Register(aggregators.NewProxyHighErrorRate())
	eng.Register(aggregators.NewProxyCostSpike())
	eng.Register(aggregators.NewProxyHookFailureRate())
	eng.Register(aggregators.NewProxyHookTimeoutRate())
	eng.Register(aggregators.NewProxyRateLimitExceeded())
	eng.Register(aggregators.NewProxyQuotaRuntimeExceeded())
	eng.Register(aggregators.NewProxyRoutingNoMatch())
	eng.Register(aggregators.NewAuthInvalidKeyBurst())
	eng.Register(aggregators.NewProviderUpstreamError())
	eng.Register(aggregators.NewProviderHighLatencyPercentile())
	eng.Register(aggregators.NewModelRateLimitedResponses())
	eng.Register(aggregators.NewCredentialAuthFailuresCascade())
	eng.Register(aggregators.NewVKLatencyDegradation())
	eng.Register(aggregators.NewVKTokenUsageSpike())
	eng.Register(aggregators.NewComplianceHookExecutionTimeoutSurge())
	eng.Register(aggregators.NewCompliancePayloadCaptureFailureRate())
	sched.Register(eng)
	logger.Info("alerteval engine registered",
		"jobId", alerteval.EngineJobID,
		"tickSec", cfg.Scheduler.AlertEval.EngineTickSec,
		"aggregators", 19,
		"consumerGroup", alerteval.ConsumerGroup)
}
