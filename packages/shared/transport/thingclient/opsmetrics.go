package thingclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// Wire-format constants for the Thing→Hub ops-metrics + diag channel.
const (
	msgTypeMetricsSample = "metrics_sample"
	msgTypeDiagEvent     = "diag_event"
	msgTypeStaticInfo    = "static_info"
)

// metricsSampleEnvelope is the on-wire shape of a metrics_sample message.
// The envelope is FLAT: "type" sits next to the SampleBatch fields, not
// wrapped in a "payload" sub-object. SampleBatch is embedded so its JSON
// tags (thingId, sampledAt, samples) flow through unchanged.
type metricsSampleEnvelope struct {
	Type string `json:"type"`
	opsmetrics.SampleBatch
}

// diagEventEnvelope is the on-wire shape of a diag_event message.
// The envelope is FLAT — same flattening pattern as metricsSampleEnvelope.
// DiagEvent is embedded so all its JSON tags carry through.
type diagEventEnvelope struct {
	Type string `json:"type"`
	opsmetrics.DiagEvent
}

// PushMetricsSample serializes a SampleBatch into a metrics_sample message
// and queues it on the WebSocket outbox. Returns an error if the batch
// cannot be marshaled or the outbox is stalled past the sendBytes timeout.
//
// The ctx parameter is currently advisory: the underlying outbox queue uses
// its own 5s send timeout. Callers should treat this as a best-effort,
// non-blocking publisher — drops are visible via the
// nexus_thingclient_outbox_dropped_total{msg_type="metrics_sample"} counter
// and the Hub-side metrics.dropped_total counter on backpressure.
func (c *Client) PushMetricsSample(_ context.Context, batch opsmetrics.SampleBatch) error {
	env := metricsSampleEnvelope{
		Type:        msgTypeMetricsSample,
		SampleBatch: batch,
	}
	data, err := json.Marshal(env)
	if err != nil {
		c.logger.Error("Failed to marshal metrics_sample",
			slog.String("event", "marshal_error"),
			slog.String("msg_type", msgTypeMetricsSample),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("marshal metrics_sample: %w", err)
	}
	return c.sendBytes(data, msgTypeMetricsSample)
}

// staticInfoEnvelope is the on-wire shape of a static_info message.
// The envelope is flat — "type" sits next to the StaticInfo fields.
// ThingID lets Hub log-route the payload (the WS-authenticated identity is
// authoritative for storage, but ThingID in the body matches the
// metrics_sample contract for consistency on HTTP-fallback paths).
type staticInfoEnvelope struct {
	Type    string                `json:"type"`
	ThingID string                `json:"thingId"`
	Static  opsmetrics.StaticInfo `json:"staticInfo"`
}

// UpdateStaticInfo serializes a StaticInfo payload into a static_info
// message and queues it on the WebSocket outbox. The Hub merges the payload
// into thing.metadata.staticInfo via jsonb_set; existing metadata keys
// (deviceTokenHash, diagModeUntil, …) are preserved.
//
// As with PushMetricsSample this is best-effort. Drops are visible via
// outbox_dropped_total{msg_type="static_info"}; the typical caller invokes
// this once at startup and again on each Hub reconnect via OnReconnect.
//
// Today static_info flows over WebSocket only — when the client is in
// HTTP-fallback mode the message is queued on outCh and will be flushed
// once the WS pump rebinds. Operators relying on static_info during a
// prolonged Hub-WS outage should fall back to scraping /metrics for
// process identity until the WS recovers.
func (c *Client) UpdateStaticInfo(_ context.Context, info opsmetrics.StaticInfo) error {
	env := staticInfoEnvelope{
		Type:    msgTypeStaticInfo,
		ThingID: c.cfg.ThingID,
		Static:  info,
	}
	data, err := json.Marshal(env)
	if err != nil {
		c.logger.Error("Failed to marshal static_info",
			slog.String("event", "marshal_error"),
			slog.String("msg_type", msgTypeStaticInfo),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("marshal static_info: %w", err)
	}
	return c.sendBytes(data, msgTypeStaticInfo)
}

// PushDiagEvent serializes a DiagEvent into a diag_event message and queues
// it on the WebSocket outbox. Returns an error if the event cannot be
// marshaled or the outbox is stalled past the sendBytes timeout.
//
// As with PushMetricsSample this is a best-effort publisher; drops are
// visible via outbox_dropped_total{msg_type="diag_event"}.
func (c *Client) PushDiagEvent(_ context.Context, event opsmetrics.DiagEvent) error {
	env := diagEventEnvelope{
		Type:      msgTypeDiagEvent,
		DiagEvent: event,
	}
	data, err := json.Marshal(env)
	if err != nil {
		c.logger.Error("Failed to marshal diag_event",
			slog.String("event", "marshal_error"),
			slog.String("msg_type", msgTypeDiagEvent),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("marshal diag_event: %w", err)
	}
	return c.sendBytes(data, msgTypeDiagEvent)
}
