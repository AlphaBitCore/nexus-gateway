package queue

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
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
// Overflow: the channel is bounded (default 4096) and a full channel runs the CONFIGURED
// overflow policy — see writer_overflow.go and shared/audit/lossmode. This comment previously
// said Enqueue "drops the event with a WARN log", and that was true when dropping was the only
// thing it could do; it is now one of four selectable modes, and the shipped default (spill)
// writes the row durably off the caller's goroutine instead. Drops and durable overflow writes
// are counted separately — Drops() and OverflowWrites() — because "under pressure but losing
// nothing" and "discarding records" are very different situations for an operator to see.
//
// Crash safety: events sitting in the channel at hard-crash time are
// lost (the durability boundary is sqlite, not the channel). Close() flushes pending events
// synchronously so graceful shutdown does not drop. Whether overflow is allowed to lose records
// is now a configuration question rather than a property of this type.
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
	// overflowWrites counts events that took the DURABLE overflow path instead of the
	// batching channel. Reported beside drops so "under pressure, nothing lost" is
	// distinguishable from "records discarded" — the old single counter could not tell
	// those apart.
	overflowWrites atomic.Int64
	// lossMode is the audit-overflow policy (shared/audit/lossmode); overflowCh is the
	// bounded relief buffer its durable arms hand work to. See writer_overflow.go.
	lossMode   lossmode.Mode
	overflowCh chan event.Event
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
		overflowCh:    make(chan event.Event, overflowQueueDepth),
		// Spill, not the shared no-loss default, and the reason is CLAUDE.md's agent rule:
		// the macOS network extension sits in the host's outbound packet path, so a no-loss
		// mode — which must block until the record is durable, and a SQLite write under WAL
		// contention can wait out the 5 s busy_timeout — would turn audit bookkeeping into a
		// frozen network. Spill does the durable write off the caller's goroutine and counts
		// a drop only when that path is also saturated. The no-loss modes stay selectable via
		// WithLossMode for a deployment that wants them and accepts the stalls.
		lossMode: lossmode.Spill,
	}
	w.startOverflowWorker()
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
// A full channel runs the configured overflow policy rather than always dropping. Every arm of
// that policy is BOUNDED: CLAUDE.md forbids stalling the host path, so even the no-loss modes give
// up after a short window here and count the loss rather than freezing the user's network.
func (w *QueueWriter) Enqueue(e sharedaudit.AuditEvent) {
	if w == nil || w.queue == nil {
		return
	}
	row := w.buildRow(e)
	select {
	case w.ch <- row:
		// queued for the background flush loop — returns immediately
	default:
		// Channel full: run the configured overflow policy rather than always dropping.
		// See writer_overflow.go — this branch used to BE lossmode.Drop with no way to
		// select anything else.
		w.handleOverflow(row)
	}
}

// Drops returns the cumulative drop counter for Diagnostics surfacing.
func (w *QueueWriter) Drops() int64 {
	if w == nil {
		return 0
	}
	return w.drops.Load()
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
		ID:                e.ID,
		TraceID:           e.TraceID,
		ExternalRequestID: e.ExternalRequestID,
		Timestamp:         e.Timestamp,
		SourceIP:          e.SourceIP,
		TargetHost:        e.TargetHost,
		Method:            e.Method,
		Path:              e.Path,
		StatusCode:        statusCode,
		LatencyMs:         e.LatencyMs,
		Action:            deriveAction(e),
		HookDecision:      e.RequestHookDecision,
		HookReason:        hookReason,
		HookReasonCode:    hookReasonCode,
		ComplianceTags:    e.ComplianceTags,
		BumpStatus:        e.BumpStatus,
		ProviderName:      e.Provider,
		ModelName:         e.Model,
		ApiKeyClass:       e.APIKeyClass,
		ApiKeyFingerprint: e.APIKeyFingerprint,
		// Domain-matched adapter id → persisted as ingress_format and used by
		// the detail drawer's view-time recompute as the authoritative adapter.
		IngressFormat:         e.IngressFormat,
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
		// action, so these are the governed copies; the relocated
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
