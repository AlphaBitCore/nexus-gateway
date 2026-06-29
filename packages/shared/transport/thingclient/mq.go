package thingclient

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"sync"
	"time"
)

const (
	bufferRetryInterval = 5 * time.Second
)

// PublishEvent publishes an event to the specified MQ queue.
// For server-side Things only (MQProducer must be set in Config).
// If MQ is unavailable, the event is buffered locally for retry.
func (c *Client) PublishEvent(ctx context.Context, queue string, data []byte) error {
	if c.cfg.MQProducer == nil {
		return fmt.Errorf("thingclient: MQ producer not configured (agent Things should use UploadAudit)")
	}

	err := c.cfg.MQProducer.Enqueue(ctx, queue, data)
	if err != nil {
		c.bufferEvent(queue, data)
		return fmt.Errorf("thingclient: enqueue to %s failed, buffered: %w", queue, err)
	}

	c.promMetrics.mqPublished.WithLabelValues(queue).Inc()
	return nil
}

// --- Ring Buffer ---

// bufferedEvent holds an event waiting for MQ retry.
type bufferedEvent struct {
	Queue string
	Data  []byte
}

// ringBuffer is a bounded FIFO buffer that drops oldest events on overflow.
// All methods are goroutine-safe.
type ringBuffer struct {
	mu      sync.Mutex
	buf     []bufferedEvent
	head    int
	tail    int
	count   int
	cap     int
	dropped int64
	metrics *clientMetrics
	logger  *slog.Logger
}

func newRingBuffer(capacity int, metrics *clientMetrics, logger *slog.Logger) *ringBuffer {
	return &ringBuffer{
		buf:     make([]bufferedEvent, capacity),
		cap:     capacity,
		metrics: metrics,
		logger:  logger,
	}
}

// Push adds an event to the buffer. If full, drops the oldest event.
func (rb *ringBuffer) Push(evt bufferedEvent) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.count == rb.cap {
		rb.head = (rb.head + 1) % rb.cap
		rb.count--
		rb.dropped++
		rb.metrics.mqDropped.Inc()
		rb.logger.Warn("MQ buffer full, dropping oldest event",
			slog.String("event", "mq_buffer_overflow"),
			slog.Int64("total_dropped", rb.dropped),
		)
	}

	rb.buf[rb.tail] = evt
	rb.tail = (rb.tail + 1) % rb.cap
	rb.count++
	rb.metrics.mqBufferSize.Set(float64(rb.count))
}

// Pop removes and returns the oldest event. Returns false if empty.
func (rb *ringBuffer) Pop() (bufferedEvent, bool) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.count == 0 {
		return bufferedEvent{}, false
	}

	evt := rb.buf[rb.head]
	rb.buf[rb.head] = bufferedEvent{}
	rb.head = (rb.head + 1) % rb.cap
	rb.count--
	rb.metrics.mqBufferSize.Set(float64(rb.count))
	return evt, true
}

// Len returns the current number of buffered events.
func (rb *ringBuffer) Len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.count
}

// DrainAll returns all buffered events and empties the buffer.
func (rb *ringBuffer) DrainAll() []bufferedEvent {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.count == 0 {
		return nil
	}

	events := make([]bufferedEvent, 0, rb.count)
	for rb.count > 0 {
		events = append(events, rb.buf[rb.head])
		rb.buf[rb.head] = bufferedEvent{}
		rb.head = (rb.head + 1) % rb.cap
		rb.count--
	}

	rb.metrics.mqBufferSize.Set(0)
	return events
}

// --- Buffer Drain ---

func (c *Client) bufferEvent(queue string, data []byte) {
	if c.mqBuffer == nil {
		return
	}

	c.mqBuffer.Push(bufferedEvent{
		Queue: queue,
		Data:  data,
	})

	c.logger.Debug("Event buffered for MQ retry",
		slog.String("event", "mq_buffered"),
		slog.String("queue", queue),
		slog.Int("buffer_size", c.mqBuffer.Len()),
	)
}

// startBufferDrainer launches the background goroutine that retries buffered events.
func (c *Client) startBufferDrainer(ctx context.Context) {
	if c.cfg.MQProducer == nil {
		return
	}

	c.mqBuffer = newRingBuffer(c.cfg.MQBufferSize, c.promMetrics, c.logger)

	go c.bufferDrainLoop(ctx)
}

func (c *Client) bufferDrainLoop(ctx context.Context) {
	ticker := time.NewTicker(bufferRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.drainBuffer(ctx)
		}
	}
}

// drainBuffer attempts to publish all buffered events. Stops on first failure.
func (c *Client) drainBuffer(ctx context.Context) {
	if c.mqBuffer == nil || c.mqBuffer.Len() == 0 {
		return
	}

	drained := 0
	for {
		evt, ok := c.mqBuffer.Pop()
		if !ok {
			break
		}

		err := c.cfg.MQProducer.Enqueue(ctx, evt.Queue, evt.Data)
		if err != nil {
			c.mqBuffer.Push(evt)
			c.logger.Debug("MQ still unavailable, will retry later",
				slog.String("event", "mq_drain_failed"),
				slog.Int("remaining", c.mqBuffer.Len()),
				slog.String("error", err.Error()),
			)
			break
		}

		c.promMetrics.mqPublished.WithLabelValues(evt.Queue).Inc()
		drained++
	}

	if drained > 0 {
		c.logger.Info("Drained buffered events to MQ",
			slog.String("event", "mq_buffer_drained"),
			slog.Int("drained", drained),
			slog.Int("remaining", c.mqBuffer.Len()),
		)
	}
}

// flushMQBuffer drains all remaining events during graceful shutdown.
func (c *Client) flushMQBuffer(ctx context.Context) {
	if c.mqBuffer == nil || c.mqBuffer.Len() == 0 {
		return
	}

	events := c.mqBuffer.DrainAll()
	c.logger.Info("Flushing MQ buffer on shutdown",
		slog.String("event", "mq_buffer_flush"),
		slog.Int("events", len(events)),
	)

	for _, evt := range events {
		if ctx.Err() != nil {
			c.logger.Warn("Shutdown timeout, dropping remaining buffered events",
				slog.String("event", "mq_flush_timeout"),
				slog.Int("dropped", len(events)),
			)
			return
		}

		publishCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.cfg.MQProducer.Enqueue(publishCtx, evt.Queue, evt.Data)
		cancel()

		if err != nil {
			c.logger.Warn("Failed to flush buffered event on shutdown",
				slog.String("event", "mq_flush_failed"),
				slog.String("queue", evt.Queue),
				slog.String("error", err.Error()),
			)
		}
	}
}

// --- Agent Audit Upload ---

// AuditBatchResponse is the response from Hub's audit upload endpoint.
type AuditBatchResponse struct {
	Ack          bool     `json:"ack"`
	ConfirmedIDs []string `json:"confirmedIds"`
}

// UploadAgentAudit uploads agent-captured audit events to Hub via HTTP
// POST to /api/internal/things/agent-audit. Wire shape is a raw JSON
// array of AgentAuditEvent objects (NOT the {thingId,events} envelope
// used by /api/internal/things/audit — the agent-audit handler binds
// directly into []AgentAuditEvent and rejects envelopes with HTTP 400).
//
// 2026-05-24 fix: the old `UploadAudit` POSTed agent batches to the
// compliance-proxy `/things/audit` endpoint whose AuditUpload handler
// silently accepted the events but discarded the PayloadRequest /
// PayloadResponse / *SpillRef fields its struct doesn't carry — Hub
// then wrote traffic_event_payload rows with NULL bodies for 100% of
// agent inspect rows. Routing agent uploads to agent-audit fixes
// body persistence end-to-end. The legacy UploadAudit is preserved
// below for any caller still on the cp-shape envelope (none today).
func (c *Client) UploadAgentAudit(ctx context.Context, events []byte) (*AuditBatchResponse, error) {
	hc := c.getHTTPClient()
	// Agent-audit handler expects a raw JSON array; no envelope wrap.
	body, status, err := hc.do(ctx, "POST", "/api/internal/things/agent-audit", json.RawMessage(events))
	if err != nil {
		return nil, fmt.Errorf("agent-audit upload: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("agent-audit upload: HTTP %d: %s", status, string(body))
	}
	var resp AuditBatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("agent-audit upload: unmarshal response: %w", err)
	}
	c.promMetrics.httpFallbackReqs.WithLabelValues("agent_audit_upload").Inc()
	c.logger.Debug("Agent-audit batch uploaded", slog.String("event", "agent_audit_uploaded"))
	return &resp, nil
}

// UploadAgentAuditWithRetry wraps UploadAgentAudit with exponential backoff.
func (c *Client) UploadAgentAuditWithRetry(ctx context.Context, events []byte, maxRetries int) (*AuditBatchResponse, error) {
	if maxRetries <= 0 {
		maxRetries = 3
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		resp, err := c.UploadAgentAudit(ctx, events)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		c.logger.Warn("Agent-audit upload failed, retrying",
			slog.String("event", "agent_audit_upload_retry"),
			slog.Int("attempt", attempt+1),
			slog.Int("max_retries", maxRetries),
			slog.String("error", err.Error()),
		)
	}
	return nil, fmt.Errorf("agent-audit upload failed after %d retries: %w", maxRetries, lastErr)
}

// UploadAudit uploads a batch of audit events to Hub via HTTP POST.
// LEGACY: routes to /api/internal/things/audit which is the
// compliance-proxy handler — NOT what agent should use. Kept for
// backward-compat; agent callers MUST use UploadAgentAuditWithRetry.
//
// Wire shape: Hub's /api/internal/things/audit handler expects the body
// to be a JSON object `{thingId, events:[...]}` (see
// nexus-hub/internal/handler/internal_things.go:AuditUpload). Sending
// the raw events array — sending the raw array instead of the
// wrapped envelope produces silently-failing HTTP 400 "invalid request
// body" even after the X-Thing-Id header is present.
func (c *Client) UploadAudit(ctx context.Context, events []byte) (*AuditBatchResponse, error) {
	envelope := struct {
		ThingID string          `json:"thingId"`
		Events  json.RawMessage `json:"events"`
	}{
		ThingID: c.cfg.ThingID,
		Events:  json.RawMessage(events),
	}
	hc := c.getHTTPClient()
	body, status, err := hc.do(ctx, "POST", "/api/internal/things/audit", envelope)
	if err != nil {
		return nil, fmt.Errorf("audit upload: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("audit upload: HTTP %d: %s", status, string(body))
	}

	var resp AuditBatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("audit upload: unmarshal response: %w", err)
	}

	c.promMetrics.httpFallbackReqs.WithLabelValues("audit_upload").Inc()

	c.logger.Debug("Audit batch uploaded",
		slog.String("event", "audit_uploaded"),
		slog.Int("confirmed", len(resp.ConfirmedIDs)),
	)

	return &resp, nil
}

// UploadAuditWithRetry uploads audit events with exponential backoff retry.
func (c *Client) UploadAuditWithRetry(ctx context.Context, events []byte, maxRetries int) (*AuditBatchResponse, error) {
	if maxRetries <= 0 {
		maxRetries = 3
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		resp, err := c.UploadAudit(ctx, events)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		c.logger.Warn("Audit upload failed, retrying",
			slog.String("event", "audit_upload_retry"),
			slog.Int("attempt", attempt+1),
			slog.Int("max_retries", maxRetries),
			slog.String("error", err.Error()),
		)
	}

	return nil, fmt.Errorf("audit upload failed after %d retries: %w", maxRetries, lastErr)
}
