// observability.go — audit writer, metrics, OTel, payload capture config wiring.
package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	epMetrics "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillsweep"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// InitOtelConfig builds a telemetry.Config from file config and system_metadata.
func InitOtelConfig(ctx context.Context, db *store.DB, cfg *config.Config) telemetry.Config {
	result := telemetry.Config{
		ServiceName: "nexus-ai-gateway",
	}
	if cfg.Otel.Endpoint != "" {
		result.Endpoint = cfg.Otel.Endpoint
	}
	if cfg.Otel.ServiceName != "" {
		result.ServiceName = cfg.Otel.ServiceName
	}
	if db == nil {
		return result
	}
	raw, err := db.GetSystemMetadata(ctx, "observability.config")
	if err != nil || raw == nil {
		return result
	}
	var stored struct {
		OtelEnabled  bool    `json:"otelEnabled"`
		SamplingRate float64 `json:"samplingRate"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return result
	}
	result.Enabled = stored.OtelEnabled
	result.SamplingRate = stored.SamplingRate
	return result
}

// InitAuditWriter creates the audit writer and wires spill store + normalizer.
// Returns the constructed normalize registry alongside the writer so request-
// path consumers (proxy handler → request context → L2 semantic cache) can
// share the same registry instead of building a second one. Without sharing,
// proxy.Deps.NormalizeRegistry stayed nil, rctxFull.Normalized() always
// returned nil, and L2 silently skipped every lookup with canonicalMsgs_len=0.
func InitAuditWriter(
	mqProducer mq.Producer,
	spillCfg spillfactory.FactoryConfig,
	payloadCaptureStore *payloadcapture.Store,
	opsReg *registry.Registry,
	logger *slog.Logger,
) (*audit.Writer, *normcore.Registry, error) {
	auditWriter := audit.NewWriter(mqProducer, "nexus.event.ai-traffic", opsReg, logger)
	spillStore, err := spillfactory.New(spillCfg, logger)
	if err != nil {
		return nil, nil, err
	}
	if spillStore != nil {
		auditWriter.WithSpillStore(spillStore)
		// Process-lifetime sweep so the backend's retention horizon and
		// total-size cap are enforced. The store is per-process, so each
		// owner sweeps its own; for a shared S3 bucket the sweeps are
		// idempotent across services.
		go spillsweep.Run(context.Background(), spillStore, spillsweep.Options{
			Retention: spillCfg.RetentionHorizon(),
		}, logger)
	}

	normalizeRegistry := normcore.NewRegistry()
	normcodecs.RegisterDefaultAIBuiltins(normalizeRegistry)
	adapters.RegisterTier1AdapterNormalizers(normalizeRegistry)
	extract.WireTier2(normalizeRegistry)
	normalizeRegistry.Freeze()
	normalizeMetrics := normcore.NewMetrics(prometheus.DefaultRegisterer, "nexus")
	auditWriter.WithNormalizer(audit.NormalizeFn(normcore.BuildAuditFn(normalizeRegistry, normalizeMetrics)))
	slog.Info("normalize registry wired", "adapters", normalizeRegistry.All())

	auditWriter.WithPayloadCaptureStore(payloadCaptureStore)
	return auditWriter, normalizeRegistry, nil
}

// InitMetricsRecorder creates the AI Gateway business metrics recorder.
func InitMetricsRecorder(opsReg *registry.Registry) *epMetrics.Recorder {
	return epMetrics.NewRecorder(opsReg)
}

// LoadPayloadCaptureConfig reads system_metadata["payload_capture.config"]
// via the AI Gateway's DB handle and returns the decoded Config. A missing
// row or a bad JSON blob yields the conservative default (capture flags
// off, 256 KiB inline cutoff, 10 MiB network read caps).
func LoadPayloadCaptureConfig(ctx context.Context, db *store.DB) (payloadcapture.Config, error) {
	if db == nil {
		return payloadcapture.DefaultConfig(), nil
	}
	raw, err := db.GetSystemMetadata(ctx, "payload_capture.config")
	if err != nil {
		return payloadcapture.DefaultConfig(), fmt.Errorf("payload capture: read system_metadata: %w", err)
	}
	if raw == nil {
		return payloadcapture.DefaultConfig(), nil
	}
	cfg, err := payloadcapture.DecodeConfigJSON(raw)
	if err != nil {
		return payloadcapture.DefaultConfig(), fmt.Errorf("payload capture: %w", err)
	}
	return cfg, nil
}
