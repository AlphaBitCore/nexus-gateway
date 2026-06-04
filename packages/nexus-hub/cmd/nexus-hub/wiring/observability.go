package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	selfreg "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/self/reg"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	sharedopsplatform "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	sharedops "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	normalizecodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// InitDiagSink wires the Hub SlogSink so Hub's own ERROR+ slog records are
// captured as DiagEvents stored in thing_diag_event (and published to the MQ
// diag topic). Returns the updated logger (with the multi-handler installed).
//
// Emits a lifecycle "nexus-hub started" DiagEvent 600ms after startup.
func InitDiagSink(
	cfg *config.HubConfig,
	opsRes OpsMetricsResult,
	opsReg *sharedops.Registry,
	buildVersion string,
	logger *slog.Logger,
) *slog.Logger {
	diagWriter := opsRes.DiagWriter
	hubDiagPusher := &HubDiagAdapter{
		ThingID:   cfg.Hub.ID,
		ThingType: selfreg.ThingType,
		Writer:    diagWriter,
	}
	hubDiagSink := shareddiag.NewSlogSink(shareddiag.SlogSinkConfig{
		ThingClient: hubDiagPusher,
		ThingID:     cfg.Hub.ID,
		Source:      "nexus-hub",
		// Hub is always "connected" to itself — no reconnect buffer needed.
		IsWSConnected: func() bool { return true },
		OpsReg:        opsReg,
	})
	newLogger := slog.New(shareddiag.NewMultiHandler(logger.Handler(), hubDiagSink))
	slog.SetDefault(newLogger)

	// Emit Hub lifecycle start event.
	go func() {
		time.Sleep(600 * time.Millisecond)
		_ = diagWriter.Enqueue(context.Background(), cfg.Hub.ID, selfreg.ThingType, sharedops.DiagEvent{
			ThingID:    cfg.Hub.ID,
			OccurredAt: time.Now().UTC(),
			EventType:  sharedops.EventTypeLifecycle,
			Level:      sharedops.LevelInfo,
			Source:     "nexus-hub",
			Message:    "nexus-hub started",
			Attrs:      map[string]any{"version": buildVersion, "id": cfg.Hub.ID},
		})
	}()

	return newLogger
}

// OTELResult holds the tracer provider and its initial config.
type OTELResult struct {
	Provider   *telemetry.SwappableTracerProvider
	InitialCfg telemetry.Config
}

// InitOTEL creates the Hub-side SwappableTracerProvider. Returns (nil result,
// nil error) when OTEL is disabled or init fails — the error is logged as a
// warning, not returned, so Hub continues without tracing.
func InitOTEL(ctx context.Context, cfg *config.HubConfig, logger *slog.Logger) OTELResult {
	hubOtelCfg := telemetry.Config{
		Enabled:      cfg.OTEL.Enabled,
		Endpoint:     cfg.OTEL.Endpoint,
		ServiceName:  "nexus-hub",
		SamplingRate: 1.0,
	}
	tp, err := telemetry.Init(ctx, hubOtelCfg, logger)
	if err != nil {
		logger.Warn("hub OTEL init failed", "error", err)
		return OTELResult{InitialCfg: hubOtelCfg}
	}
	return OTELResult{Provider: tp, InitialCfg: hubOtelCfg}
}

// InitSelfInstrumentation starts the Hub self-sampling loop that pushes
// metrics into opsWriter at the same 15s cadence as thingclient services,
// and pushes static_info once at startup.
func InitSelfInstrumentation(
	ctx context.Context,
	cfg *config.HubConfig,
	buildVersion string,
	processStartTime time.Time,
	pool *pgxpool.Pool,
	opsReg *sharedops.Registry,
	opsRes OpsMetricsResult,
	logger *slog.Logger,
) {
	opsWriter := opsRes.Writer
	opsStaticWriter := opsRes.StaticWriter
	hubThingID := cfg.Hub.ID
	hubSampler := sharedopsplatform.NewSampler(hubThingID, processStartTime, opsReg)

	// Push static_info best-effort; failure does not block startup since
	// selfReg already inserted the thing row.
	go func() {
		time.Sleep(500 * time.Millisecond)
		ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		info := sharedopsplatform.CaptureStaticInfo(sharedopsplatform.BuildInfo{
			ServiceVersion: "nexus-hub/" + buildVersion,
			BuildSHA:       "",
			BuildTime:      "",
			StartTime:      processStartTime.Format(time.RFC3339),
			PublicURL:      cfg.PublicURL,
		})
		if err := opsStaticWriter.UpsertStaticInfo(ctxPush, hubThingID, info); err != nil {
			logger.Warn("hub static_info upsert failed at startup", "error", err)
		}
	}()

	// Per-tick sample emission. 15s mirrors the thingclient default
	// HeartbeatInterval so Hub data lands in ops_metrics_sample at the same
	// cadence as cluster services.
	const selfSampleInterval = 15 * time.Second
	go func() {
		ticker := time.NewTicker(selfSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				batch := hubSampler.Collect()
				if err := opsWriter.Enqueue(ctx, hubThingID, selfreg.ThingType, batch); err != nil {
					logger.Debug("hub self metrics_sample enqueue failed", "error", err)
				}
			}
		}
	}()
}

// InitNormalizeRegistry builds the shared/normalize registry for agent-audit
// traffic. Projects agent-uploaded request/response bytes into the canonical
// NormalizedPayload shape before publishing to MQ.
func InitNormalizeRegistry(buildVersion string) normalizecore.AuditFn {
	agentNormRegistry := normalizecore.NewRegistry()
	normalizecodecs.RegisterDefaultAIBuiltins(agentNormRegistry)
	// Tier 1: per-host adapter Normalizers (chatgpt-web / claude-web /
	// gemini-web / openai-compat / ...). Skips adapter IDs already covered by
	// RegisterDefaultAIBuiltins (anthropic, gemini).
	adapters.RegisterTier1AdapterNormalizers(agentNormRegistry)
	// Tier 2: pattern-based extraction fallback.
	extract.WireTier2(agentNormRegistry)
	agentNormRegistry.Freeze()

	agentNormMetrics := normalizecore.MustRegisterPrometheus(prometheus.DefaultRegisterer, "nexus_hub_agent")
	return normalizecore.BuildAuditFn(agentNormRegistry, agentNormMetrics)
}
