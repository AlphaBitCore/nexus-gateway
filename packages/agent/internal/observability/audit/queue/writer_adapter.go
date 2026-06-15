package queue

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// QueueWriter adapts the agent's local sqlite-backed Queue to the
// shared/audit.Writer contract. Writes are non-blocking on the inspect
// hot path: Enqueue does an O(ns) channel push, and a background flush
// loop batches every 100 events (or every 100 ms, whichever fires
// first) into a single SQLite transaction. Pre-async the writer ran
// queue.Record inline — a ~1 ms WAL fsync per row, serialized by the
// SQLite write lock under a burst of N concurrent inspect flows. That
// added N ms of tail latency to user-visible page loads.
//
// Backpressure: the channel is bounded (default 4096). When full,
// Enqueue drops the event with a WARN log + drops counter increment
// rather than block the inspect goroutine. Drops are visible on the
// Stats / Diagnostics page and indicate audit pipeline starvation
// (Hub upload backlog, disk slow, etc.) — never user-network slowdown.
//
// Crash safety: events sitting in the channel at hard-crash time are
// lost (the durability boundary is sqlite, not the channel). For a
// home / single-user agent this is acceptable — audit is best-effort,
// not transactional. Close() flushes pending events synchronously so
// graceful shutdown does not drop.
type QueueWriter struct {
	queue *Queue
	ch    chan event.Event
	done  chan struct{}
	// closeOnce guards `close(done)` so two concurrent Close() calls
	// can't double-close the channel (panic). Without this guard
	// shutdown races between the daemon's signal handler and a test's
	// `defer Close` would crash.
	closeOnce sync.Once
	wg        sync.WaitGroup
	// drops counts events dropped because the channel was full.
	// Surfaced by future Diagnostics so operators can see pipeline
	// starvation independent of Hub upload health.
	drops atomic.Int64
	// flushBatch + flushInterval control the background flush cadence.
	// Defaults: 100 events / 100 ms. Tuned so a quiet machine writes
	// every 100 ms (UI sees fresh rows fast) while a busy machine
	// hits the batch trip at <100 ms and amortizes fsync across rows.
	flushBatch    int
	flushInterval time.Duration
	// flushReq is a synchronous barrier the Flush method sends through
	// so callers can wait for a guaranteed-committed checkpoint: the
	// worker drains everything before the sentinel, commits, then
	// closes the responseCh — Flush sees the close and returns.
	flushReq chan chan struct{}
}

// NewQueueWriter returns a Writer that persists shared/audit.AuditEvent
// records into the agent's encrypted sqlite Queue via an async
// channel-buffered, batch-committed background flush loop. Callers must
// invoke Close on shutdown to drain the channel.
func NewQueueWriter(q *Queue) *QueueWriter {
	return NewQueueWriterWithOptions(q, 4096, 100, 100*time.Millisecond)
}

// NewQueueWriterWithOptions exposes the buffer / batch / interval knobs
// for tests + benchmarks. Production callers should use NewQueueWriter.
func NewQueueWriterWithOptions(q *Queue, bufferSize, flushBatch int, flushInterval time.Duration) *QueueWriter {
	w := &QueueWriter{
		queue:         q,
		ch:            make(chan event.Event, bufferSize),
		done:          make(chan struct{}),
		flushBatch:    flushBatch,
		flushInterval: flushInterval,
		flushReq:      make(chan chan struct{}, 4),
	}
	w.wg.Add(1)
	go w.flushLoop()
	return w
}

// Enqueue maps the canonical shared/audit.AuditEvent shape to the agent's
// local Event row and forwards it to the async flush loop via a bounded
// channel. Field-by-field translation:
//
//   - HookDecision is taken from RequestHookDecision (response stage stays
//     in the JSON pipeline blob; agent's table has only one top-level
//     decision column today).
//   - LatencyMs, BumpStatus, Method, Path, StatusCode flow through.
//   - Provider/Model + token usage map to the matching agent columns.
//   - PromptTokens / CompletionTokens are stored as nullable ints —
//     pointer if non-zero, nil if zero (mirrors how the MITM relay
//     pre-T33 populated them so the agent + cp wire shape matches).
//
// Non-blocking: a full channel triggers a drop + WARN log rather than
// blocking the inspect goroutine.
func (w *QueueWriter) Enqueue(e sharedaudit.AuditEvent) {
	if w == nil || w.queue == nil {
		return
	}
	row := w.buildRow(e)
	select {
	case w.ch <- row:
		// queued for the background flush loop — returns immediately
	default:
		// Channel is full — drop with a single WARN per drop so log
		// volume stays bounded under sustained pressure.
		n := w.drops.Add(1)
		slog.Warn("audit writer: channel full, dropping event",
			"event_id", row.ID,
			"target_host", row.TargetHost,
			"method", row.Method,
			"path", row.Path,
			"action", row.Action,
			"drops_total", n,
		)
	}
}

// Drops returns the cumulative drop counter for Diagnostics surfacing.
func (w *QueueWriter) Drops() int64 {
	if w == nil {
		return 0
	}
	return w.drops.Load()
}

// flushLoop consumes the channel, batches up to flushBatch events into
// a single sqlite transaction, and commits on either the batch-size or
// the flushInterval trigger — whichever fires first. Exits on done
// signal after draining whatever is still in the channel.
func (w *QueueWriter) flushLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()
	batch := make([]event.Event, 0, w.flushBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.queue.RecordBatch(batch); err != nil {
			slog.Warn("audit writer: batch insert failed — rows dropped",
				"error", err,
				"batch_size", len(batch),
			)
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-w.done:
			// Drain remaining events (whatever was in the channel at
			// Close time) then return.
			for {
				select {
				case e := <-w.ch:
					batch = append(batch, e)
					if len(batch) >= w.flushBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-w.ch:
			batch = append(batch, e)
			if len(batch) >= w.flushBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case resp := <-w.flushReq:
			// Synchronous barrier: drain anything that's still
			// in the channel (so the flush covers events that
			// arrived between Flush() and this case firing),
			// then flush, then signal the caller.
			for {
				select {
				case e := <-w.ch:
					batch = append(batch, e)
					if len(batch) >= w.flushBatch {
						flush()
					}
				default:
					flush()
					close(resp)
					goto nextIter
				}
			}
		nextIter:
		}
	}
}

func (w *QueueWriter) buildRow(e sharedaudit.AuditEvent) event.Event {
	statusCode := 0
	if e.StatusCode != nil {
		statusCode = *e.StatusCode
	}
	hookReason := ""
	if e.RequestHookReason != nil {
		hookReason = *e.RequestHookReason
	}
	hookReasonCode := ""
	if e.RequestHookReasonCode != nil {
		hookReasonCode = *e.RequestHookReasonCode
	}
	var promptTokens *int
	if e.PromptTokens > 0 {
		v := int(e.PromptTokens)
		promptTokens = &v
	}
	var completionTokens *int
	if e.CompletionTokens > 0 {
		v := int(e.CompletionTokens)
		completionTokens = &v
	}
	hooksPipeline := json.RawMessage(nil)
	if len(e.RequestHooksPipeline) > 0 {
		hooksPipeline = json.RawMessage(e.RequestHooksPipeline)
	} else if len(e.ResponseHooksPipeline) > 0 {
		hooksPipeline = json.RawMessage(e.ResponseHooksPipeline)
	}
	// Captured body bytes: shared/audit.Body wraps either inline bytes
	// (small body) or a SpillRef (oversize body the engine wrote to the
	// localfs spill store). Both are persisted: the inline bytes go to the
	// payload_request/response BLOB columns, and the SpillRef is JSON-encoded
	// into request_spill_ref/response_spill_ref. The drain step uploads the
	// localfs-spilled body to S3 and swaps the ref before shipping to Hub;
	// the agent's own detail view reads the localfs body back from the ref.
	// Empty (capture disabled) → both nil so SQLite stores NULL.
	row := event.Event{
		ID:                    e.ID,
		TraceID:               e.TraceID,
		Timestamp:             e.Timestamp,
		SourceIP:              e.SourceIP,
		TargetHost:            e.TargetHost,
		Method:                e.Method,
		Path:                  e.Path,
		StatusCode:            statusCode,
		LatencyMs:             e.LatencyMs,
		Action:                deriveAction(e),
		HookDecision:          e.RequestHookDecision,
		HookReason:            hookReason,
		HookReasonCode:        hookReasonCode,
		ComplianceTags:        e.ComplianceTags,
		BumpStatus:            e.BumpStatus,
		ProviderName:          e.Provider,
		ModelName:             e.Model,
		ApiKeyClass:           e.APIKeyClass,
		ApiKeyFingerprint:     e.APIKeyFingerprint,
		PromptTokens:          promptTokens,
		CompletionTokens:      completionTokens,
		UsageExtractionStatus: e.UsageExtractionStatus,
		PayloadRequest:        e.RequestBody.InlineBytes,
		PayloadResponse:       e.ResponseBody.InlineBytes,
		RequestSpillRef:       e.RequestBody.SpillRef,
		ResponseSpillRef:      e.ResponseBody.SpillRef,
		HooksPipeline:         hooksPipeline,
		ErrorCode:             e.ErrorCode,
		ErrorReason:           e.ErrorReason,
		DomainRuleID:          e.DomainRuleID,
		PathAction:            e.PathAction,
		// Prefer the human-readable process name; fall back to the
		// bundle ID so the App column is never blank if either field
		// has data.
		SourceProcess: firstNonEmptySrc(e.SourceProcess, e.SourceProcessBundle),
		// Pre-computed NormalizedPayload JSON from the
		// shared/audit.AuditEvent propagated down to the agent.Event
		// row so the SQLite normalized_request / normalized_response
		// columns persist. The emitter already applied the stage's
		// storageAction, so these are the governed copies; the relocated
		// redaction spans ride alongside. nil/empty when no AI adapter
		// matched (or the row is unredacted, for the spans).
		NormalizedRequest:      e.RequestNormalized,
		NormalizedResponse:     e.ResponseNormalized,
		RequestRedactionSpans:  e.RequestRedactionSpans,
		ResponseRedactionSpans: e.ResponseRedactionSpans,
	}
	if row.Timestamp.IsZero() {
		row.Timestamp = time.Now().UTC()
	}
	return row
}

// firstNonEmptySrc picks the human-readable source name when present,
// else falls back to the bundle ID. Both come from
// NEAppProxyFlow.metaData via tlsbump.WithProcessInfo.
func firstNonEmptySrc(name, bundle string) string {
	if name != "" {
		return name
	}
	return bundle
}

// deriveAction translates a shared/audit decision into the agent's coarser
// "action" enum used on the Activity / Stats UI ("inspect" / "passthrough"
// / "deny"). Mirrors the pre-T33 MITM-relay mapping so the dashboard
// reads the same regardless of producer.
func deriveAction(e sharedaudit.AuditEvent) string {
	switch e.RequestHookDecision {
	case "REJECT_HARD", "BLOCK_SOFT":
		return "deny"
	case "":
		return "passthrough"
	default:
		return "inspect"
	}
}

// Flush sends a synchronous barrier through the flush loop's channel
// and waits for the worker to drain + commit + signal. Returns when
// every event Enqueue'd before Flush call is committed to sqlite.
// Used by integration tests + shutdown drains.
func (w *QueueWriter) Flush(ctx context.Context) error {
	if w == nil {
		return nil
	}
	resp := make(chan struct{})
	select {
	case w.flushReq <- resp:
	case <-w.done:
		return nil // writer already closed
	case <-ctx.Done():
		return ctx.Err()
	}
	// Also watch w.done in the response wait: if Close races with Flush,
	// the loop may exit between accepting flushReq and signalling resp,
	// in which case resp would never close and Flush would block forever.
	select {
	case <-resp:
		return nil
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close signals the flush loop to drain its remaining events and exit.
// Idempotent under concurrent invocation via sync.Once. After Close
// returns, the underlying Queue may be closed safely by the daemon's
// shutdown path.
func (w *QueueWriter) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(func() { close(w.done) })
	// Wait for the flush loop to finish, bounded by ctx so daemon
	// shutdown never hangs on a stuck sqlite.
	doneCh := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
