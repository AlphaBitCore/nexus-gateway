package wiring

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// AuditResult holds the components created by InitAudit.
type AuditResult struct {
	Writer audit.Writer
}

// InitAudit initializes the audit MQ writer with optional NDJSON fallback.
// Compliance-proxy publishes audit events to nexus.event.compliance; the
// Hub db-writer consumer is responsible for all traffic_event inserts.
// SIEM forwarding is centralized in Hub's traffic/siem bridge — CP no
// longer runs a local SIEM sink (see Hub-canonical-SIEM consolidation).
func InitAudit(cfg *config.Config, mqProducer mq.Producer, logger *slog.Logger) (AuditResult, error) {
	var result AuditResult

	if cfg.Audit.Enabled {
		if mqProducer == nil {
			return AuditResult{}, fmt.Errorf("audit enabled but MQ producer is nil — compliance-proxy writes audit events to nexus.event.compliance; configure mq.* in compliance-proxy.config.yaml")
		}

		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "unknown"
		}

		// NDJSON fallback writer — spills to local disk when MQ publish fails.
		var ndjsonWriter *audit.NDJSONWriter
		if cfg.Audit.NDJSON.Enabled && cfg.Audit.NDJSON.Dir != "" {
			var err error
			ndjsonWriter, err = audit.NewNDJSONWriter(
				cfg.Audit.NDJSON.Dir,
				hostname,
				cfg.Audit.NDJSON.MaxFileSizeMB,
				cfg.Audit.NDJSON.MaxTotalSizeMB,
				logger,
			)
			if err != nil {
				slog.Warn("NDJSON fallback writer init failed", "error", err)
			}
		}

		batchSize := cfg.Audit.Batch.Size
		if batchSize <= 0 {
			batchSize = 10
		}
		flushMs := cfg.Audit.Batch.FlushIntervalMs
		if flushMs <= 0 {
			flushMs = 500
		}
		channelBuf := cfg.Audit.Batch.ChannelBufferSize
		if channelBuf <= 0 {
			channelBuf = 1000
		}

		mqWriter := audit.NewMQBatchWriter(
			mqProducer,
			"nexus.event.compliance",
			batchSize,
			time.Duration(flushMs)*time.Millisecond,
			channelBuf,
			ndjsonWriter,
			logger,
		)
		result.Writer = mqWriter
		slog.Info("audit MQ writer initialized",
			"queue", "nexus.event.compliance",
			"batchSize", batchSize,
			"flushIntervalMs", flushMs,
		)
	}

	return result, nil
}
