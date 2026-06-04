package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// SIEMForwarderConfig holds configuration for the SIEM forwarder consumer.
type SIEMForwarderConfig struct {
	Enabled       bool          `yaml:"enabled"`
	BatchSize     int           `yaml:"batchSize"`
	FlushInterval time.Duration `yaml:"flushInterval"`
	EventTypes    []string      `yaml:"eventTypes"`
}

type pendingSIEMMessage struct {
	event siem.Event
	msg   *mq.Message
}

// SIEMQueues lists all 4 queues the SIEM forwarder reads from (3 traffic + admin audit).
var SIEMQueues = []string{
	"nexus.event.ai-traffic",
	"nexus.event.compliance",
	"nexus.event.agent",
	"nexus.event.admin-audit",
}

const siemGroup = "hub-siem"

// SIEMForwarder consumes all event queues from MQ, classifies events, applies
// optional type filtering, batches them, and sends to an external SIEM sink.
// Consumer group: "hub-siem" (separate from hub-db-writer for fan-out).
type SIEMForwarder struct {
	mqc    mq.Consumer
	sink   siem.Sink
	cfg    SIEMForwarderConfig
	logger *slog.Logger

	consumedTotal *opsmetrics.Counter
	sentTotal     *opsmetrics.Counter
	errorsTotal   *opsmetrics.Counter
}

// NewSIEMForwarder creates a new SIEM forwarder. Call Start(ctx) to begin.
// reg powers both /metrics and the per-tick metrics_sample push; pass nil
// only in test harnesses.
func NewSIEMForwarder(
	mqc mq.Consumer,
	sink siem.Sink,
	cfg SIEMForwarderConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *SIEMForwarder {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 10 * time.Second
	}

	f := &SIEMForwarder{
		mqc:    mqc,
		sink:   sink,
		cfg:    cfg,
		logger: logger.With("component", "siem-forwarder"),
	}
	if reg != nil {
		f.consumedTotal = reg.NewCounter("siem.consumed_total", []string{"queue"})
		f.sentTotal = reg.NewCounter("siem.sent_total", []string{"result"})
		f.errorsTotal = reg.NewCounter("siem.errors_total", []string{"error_type"})
	}
	return f
}

// Start begins consuming from all 4 event queues in parallel goroutines.
// Blocks until ctx is cancelled.
func (f *SIEMForwarder) Start(ctx context.Context) error {
	for _, queue := range SIEMQueues {
		q := queue
		batch := NewBatchAccumulator[pendingSIEMMessage](f.cfg.BatchSize, f.cfg.FlushInterval, func(items []pendingSIEMMessage) error {
			return f.flush(ctx, items)
		})

		go func() {
			defer batch.Stop() //nolint:errcheck

			err := f.mqc.Consume(ctx, q, siemGroup, func(_ context.Context, msg *mq.Message) error {
				if f.consumedTotal != nil {
					f.consumedTotal.With(q).Inc()
				}

				evt, err := f.deserializeEvent(q, msg.Data)
				if err != nil {
					f.logger.Warn("SIEM: deserialize failed, dropping", "queue", q, "error", err)
					if f.errorsTotal != nil {
						f.errorsTotal.With("deserialize").Inc()
					}
					return msg.Ack()
				}

				if err := batch.Add(pendingSIEMMessage{event: evt, msg: msg}); err != nil {
					// Synchronous flush failure (batch hit maxSize and flush errored).
					// flush already invoked nakAll on this item.
					return err
				}
				// Hand ack/nak off to the batch flush path (ackAll / nakAll).
				return mq.ErrDeferAck
			})

			if err != nil && ctx.Err() == nil {
				f.logger.Error("SIEM consumer exited with error", "queue", q, "error", err)
			}
		}()
	}

	<-ctx.Done()
	return nil
}

// deserializeEvent converts raw MQ bytes into a siem.Event map, then
// classifies the event based on the source queue.
func (f *SIEMForwarder) deserializeEvent(queue string, data []byte) (siem.Event, error) {
	if queue == "nexus.event.admin-audit" {
		return f.deserializeAdminEvent(data)
	}
	return f.deserializeTrafficEvent(data)
}

func (f *SIEMForwarder) deserializeTrafficEvent(data []byte) (siem.Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	raw["eventType"] = siem.ClassifyTrafficEvent(raw)
	return raw, nil
}

func (f *SIEMForwarder) deserializeAdminEvent(data []byte) (siem.Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if _, ok := raw["source"]; !ok {
		raw["source"] = "admin"
	}
	raw["eventType"] = siem.ClassifyAdminEvent(raw)
	return raw, nil
}

func (f *SIEMForwarder) flush(ctx context.Context, items []pendingSIEMMessage) error {
	events := make([]siem.Event, 0, len(items))
	for _, pm := range items {
		events = append(events, pm.event)
	}

	events = siem.FilterByEventTypes(events, f.cfg.EventTypes)

	if len(events) == 0 {
		f.ackAll(items)
		return nil
	}

	if err := f.sink.Send(ctx, events); err != nil {
		f.logger.Error("SIEM sink send failed",
			"sink", f.sink.Name(), "count", len(events), "error", err)
		if f.errorsTotal != nil {
			f.errorsTotal.With("sink_send").Inc()
		}
		if f.sentTotal != nil {
			f.sentTotal.With("error").Inc()
		}
		f.nakAll(items)
		return err
	}

	if f.sentTotal != nil {
		f.sentTotal.With("success").Inc()
	}
	f.ackAll(items)

	f.logger.Debug("forwarded events to SIEM",
		"sink", f.sink.Name(), "count", len(events))
	return nil
}

func (f *SIEMForwarder) ackAll(items []pendingSIEMMessage) {
	for _, pm := range items {
		if err := pm.msg.Ack(); err != nil {
			f.logger.Warn("ack failed", "error", err)
		}
	}
}

func (f *SIEMForwarder) nakAll(items []pendingSIEMMessage) {
	for _, pm := range items {
		if err := pm.msg.Nak(); err != nil {
			f.logger.Warn("nak failed", "error", err)
		}
	}
}
