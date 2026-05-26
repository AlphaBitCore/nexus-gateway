// Package opsmetrics is the Hub-side ingestion stack for ops metrics and
// diagnostic events emitted by Things (cluster services + agents). It owns:
//
//   - WS message handlers (HandleMetricsSample / HandleDiagEvent), wired into
//     ws.Server's per-connection MessageHandler dispatch.
//   - Two bounded-channel batch writers backed by Postgres (Sample writer →
//     metric_ops_raw via pgx CopyFrom; Diag writer → thing_diag_event via
//     batched INSERT).
//   - HTTP drain handler for crash events that the agent buffered locally
//     while disconnected (POST /api/internal/things/diag-events:batch).
//
// See §7.5 of the ops-metrics write-path design for the write-path overview and backpressure model.
package opsmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// SampleWriter is the consumer of decoded metrics_sample WS messages. The Hub
// implementation is the bounded-channel Writer in this package; tests
// substitute fakes.
//
// Enqueue MUST be non-blocking — the calling goroutine is the WS read pump
// and any blocking here directly stalls the Thing's heartbeat. Backpressure
// is signalled by silently dropping (incrementing an internal counter) and
// returning nil; an error return is reserved for hard misuse (e.g. stopped
// writer).
type SampleWriter interface {
	Enqueue(ctx context.Context, thingID, thingType string, batch opsmetrics.SampleBatch) error
}

// DiagWriter is the consumer of decoded diag_event WS messages and HTTP-drain
// events. Same non-blocking contract as SampleWriter.
type DiagWriter interface {
	Enqueue(ctx context.Context, thingID, thingType string, evt opsmetrics.DiagEvent) error
}

// StaticInfoStore persists the L2 static-identity payload to
// thing.metadata.staticInfo. Implementations write synchronously — static
// info only flows on startup + reconnect, so the volume is negligible
// compared to the metrics_sample channel and bounded-channel buffering is
// not required.
type StaticInfoStore interface {
	UpsertStaticInfo(ctx context.Context, thingID string, info opsmetrics.StaticInfo) error
}

// Handler is the message-type-keyed dispatcher invoked from ws/server.go's
// switch on IncomingMessage.Type. It is stateless beyond the writer
// references it holds, so a single Handler is shared across all connections.
type Handler struct {
	sampleWriter SampleWriter
	diagWriter   DiagWriter
	staticStore  StaticInfoStore
	log          *slog.Logger
}

// NewHandler wires the Handler with its writers + the static-info store and
// a logger. logger may be nil; in that case slog.Default is used so
// library-internal log lines still flow. staticStore may be nil — Things
// that don't yet emit static_info are unaffected, and the Handler logs +
// drops the message instead of panicking.
func NewHandler(sw SampleWriter, dw DiagWriter, ss StaticInfoStore, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		sampleWriter: sw,
		diagWriter:   dw,
		staticStore:  ss,
		log:          logger.With("component", "opsmetrics_handler"),
	}
}

// HandleMetricsSample decodes a flat metrics_sample envelope (per spec §7.1)
// and forwards the resulting SampleBatch to the SampleWriter. The envelope
// shape mirrors thingclient.metricsSampleEnvelope: type lives next to
// thingId, sampledAt, samples — there is no "payload" wrapper.
//
// thingID and thingType come from the WS connection's authenticated identity,
// not the wire payload — the payload's own thingId is informational only and
// is not trusted for routing.
func (h *Handler) HandleMetricsSample(ctx context.Context, thingID, thingType string, raw json.RawMessage) error {
	var batch opsmetrics.SampleBatch
	if err := json.Unmarshal(raw, &batch); err != nil {
		h.log.Warn("invalid metrics_sample payload",
			slog.String("thing_id", thingID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("unmarshal metrics_sample: %w", err)
	}
	if h.sampleWriter == nil {
		return fmt.Errorf("opsmetrics: sample writer not configured")
	}
	return h.sampleWriter.Enqueue(ctx, thingID, thingType, batch)
}

// HandleStaticInfo decodes a flat static_info envelope (spec §5.6 / §6.2)
// and writes the StaticInfo payload into thing.metadata.staticInfo.
// thingID / thingType come from the authenticated WS identity; the payload
// thingId is informational and not trusted for routing.
func (h *Handler) HandleStaticInfo(ctx context.Context, thingID, _ string, raw json.RawMessage) error {
	var env struct {
		Type    string                `json:"type"`
		ThingID string                `json:"thingId"`
		Static  opsmetrics.StaticInfo `json:"staticInfo"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		h.log.Warn("invalid static_info payload",
			slog.String("thing_id", thingID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("unmarshal static_info: %w", err)
	}
	if h.staticStore == nil {
		h.log.Debug("static_info store not configured, dropping payload",
			slog.String("thing_id", thingID),
		)
		return nil
	}
	if err := h.staticStore.UpsertStaticInfo(ctx, thingID, env.Static); err != nil {
		h.log.Error("static_info upsert failed",
			slog.String("thing_id", thingID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("upsert static_info: %w", err)
	}
	return nil
}

// HandleDiagEvent decodes a flat diag_event envelope (spec §7.2) and forwards
// it to the DiagWriter. Same trust model as HandleMetricsSample: thingID /
// thingType are pulled from the authenticated WS identity, not the payload.
func (h *Handler) HandleDiagEvent(ctx context.Context, thingID, thingType string, raw json.RawMessage) error {
	var evt opsmetrics.DiagEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		h.log.Warn("invalid diag_event payload",
			slog.String("thing_id", thingID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("unmarshal diag_event: %w", err)
	}
	if h.diagWriter == nil {
		return fmt.Errorf("opsmetrics: diag writer not configured")
	}
	return h.diagWriter.Enqueue(ctx, thingID, thingType, evt)
}
