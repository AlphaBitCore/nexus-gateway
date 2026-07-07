// observability.go — audit writer, metrics, OTel, payload capture config wiring.
package wiring

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	epMetrics "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillsweep"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	sharednormalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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
	auditCfg config.AuditConfig,
	payloadCaptureStore *payloadcapture.Store,
	opsReg *registry.Registry,
	logger *slog.Logger,
) (*audit.Writer, *normcore.Registry, error) {
	auditWriter := audit.NewWriter(mqProducer, "nexus.event.ai-traffic", opsReg, logger)

	// Bound the in-heap audit record buffer (overflow → durable spill). Each queued
	// record pins its pooled ~50 KB body until marshaled, so this cap is the primary
	// control over the audit side-path's gw heap (10000 default holds the body pool
	// near ~1 GB vs ~5 GB at the former hard-coded 50000, same spill rate).
	auditWriter.WithMaxQueuedRecords(auditCfg.MaxQueuedRecords)
	// Overflow policy: config default spillblock — zero-loss (durable spool, then
	// bounded back-pressure only if the spool saturates), at spill's throughput.
	// spill/drop are an explicit lossy opt-out for non-compliance callers; block is
	// the empty/unknown fallback. Log the RESOLVED mode (after that fallback) so a
	// config typo that silently reverted to block is visible rather than mysterious.
	auditWriter.WithLossMode(auditCfg.LossMode)
	logger.Info("audit overflow policy resolved",
		"configured", auditCfg.LossMode, "effective", auditWriter.LossMode())

	// In-memory audit byte budget — the memory half of the bounded audit queue.
	// Same semantics as NEXUS_EVENTS_MAX_BYTES: empty/"auto" auto-sizes to ~15% of
	// available RAM; an explicit size ("8GB") pins it. Log the RESOLVED value so
	// operators see what the box defaulted to and how to pin it.
	auditWriter.WithMemMaxBytes(auditCfg.MemMaxBytes)
	logger.Info("audit memory budget resolved",
		"configured", auditCfg.MemMaxBytes,
		"effective_bytes", auditWriter.MemBudgetBytes(),
		"effective_gib", float64(auditWriter.MemBudgetBytes())/(1<<30),
		"override", "set AI_GATEWAY_AUDIT_MEM_MAX_BYTES=<size> (e.g. 8GB) to pin a fixed value")

	// End-to-end zstd compression of large captured bodies: the producer marks
	// large bodies for compression in NewInlineBody and compresses lazily in the
	// async marshal worker (off the request path); the Hub persists the compressed
	// bytes verbatim and the CP view layer decompresses. This is the direct lever
	// on publish throughput — the audit pipeline is disk-I/O-bound at the broker.
	sharedaudit.SetInlineCompression(auditCfg.Compress, auditCfg.CompressMinBytes, auditCfg.CompressLevel)

	// Optional NDJSON record-batching on the publish path: pack marshaled records
	// into one NATS message per frame instead of one PublishAsync per record. The
	// per-record op-count is the measured audit-drain bottleneck — batching
	// recovers ~1.5x request RPS under heavy audit load. Gated OFF by default (0)
	// and enabled via NEXUS_AUDIT_FRAME_MAX_BYTES only once every Hub consumer
	// supports framed messages (the consumer is backward-compatible: a legacy
	// 1-record message is just a 1-line frame). Bound the value below the
	// deployment's NATS max_payload and the Hub frame-size cap.
	if v, perr := strconv.Atoi(os.Getenv("NEXUS_AUDIT_FRAME_MAX_BYTES")); perr == nil && v > 0 {
		auditWriter.WithFramePublish(v)
		logger.Info("audit: NDJSON frame-publish enabled", "frameMaxBytes", v)
	}

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

	// Durable NDJSON fallback for whole records when the in-memory buffer
	// overflows after backpressure. Best-effort: if the spool dir cannot be
	// created (e.g. a local dev run without /var/lib/nexus), spill is left
	// disabled and a genuine overflow becomes a loud, counted drop rather
	// than aborting startup.
	if auditCfg.SpoolDir != "" {
		ndjsonSpill, nerr := sharedndjson.New(
			auditCfg.SpoolDir, "ai-gateway", auditCfg.SpoolMaxFileMB, auditCfg.SpoolMaxTotalMB, nil,
		)
		if nerr != nil {
			logger.Warn("audit NDJSON spill disabled: cannot open spool dir",
				"dir", auditCfg.SpoolDir, "error", nerr)
		} else {
			auditWriter.WithNDJSONSpill(ndjsonSpill)
			logger.Info("audit NDJSON spill enabled", "dir", auditCfg.SpoolDir)
			// Drain half of spill-defer: replay sealed spool files back into the MQ
			// queue so a record that overflowed to disk still reaches the queryable
			// store. ON by default whenever spill is wired — a durable spool that
			// never reaches Postgres is a silent data-availability gap, not a
			// feature. Config (AuditConfig.SpillRecovery*): 0 → defaults below; a
			// negative interval disables recovery (spool drains out-of-band only).
			if auditCfg.SpillRecoveryIntervalMs >= 0 {
				interval := 2 * time.Second
				if auditCfg.SpillRecoveryIntervalMs > 0 {
					interval = time.Duration(auditCfg.SpillRecoveryIntervalMs) * time.Millisecond
				}
				pace := 50 * time.Millisecond
				if auditCfg.SpillRecoveryPaceMs > 0 {
					pace = time.Duration(auditCfg.SpillRecoveryPaceMs) * time.Millisecond
				}
				auditWriter.WithSpillRecovery(interval, pace)
				logger.Info("audit spill recovery enabled", "interval", interval, "pace", pace)
			} else {
				logger.Info("audit spill recovery disabled by config")
			}
		}
	}

	// Canonical Tier 1+1.5+2+3 assembly shared with nexus-hub, agent and
	// compliance-proxy — one builder so every data-plane service runs the
	// identical normalize chain (returned frozen).
	normalizeRegistry := sharednormalize.BuildRegistry()
	slog.Info("normalize registry built", "adapters", normalizeRegistry.All())

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
