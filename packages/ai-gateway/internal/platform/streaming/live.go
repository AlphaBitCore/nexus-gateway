package streaming

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming/format"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	sharedstreaming "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

const (
	defaultFirstInspectChars   = 400
	defaultReinspectStepChars  = 128
	defaultMaxStreamBufferSize = 8 * 1024 * 1024 // 8 MB
	defaultEventChannelSize    = 64
)

// LiveConfig configures the live streaming compliance pipeline.
type LiveConfig struct {
	FirstInspectChars  int // chars before first checkpoint (default 400)
	ReinspectStepChars int // chars between subsequent checkpoints (default 128)
	MaxBufferSize      int // max total buffer in bytes (default 8MB)
	ChannelSize        int // internal event channel buffer (default 64); mirrors shared/transport/streaming.LiveConfig.ChannelSize

	// EmitOpenAIDone controls whether the pipeline appends the OpenAI
	// `data: [DONE]\n\n` terminator after the last upstream event.
	// True for OpenAI-shape ingress clients (which use [DONE] as the
	// stream terminator); false for Anthropic / Gemini ingress clients
	// where the upstream's typed event (`message_stop`, last NDJSON
	// line) already terminates the stream — appending an extra
	// `data:` line without an `event:` field dispatches it to the
	// default "message" handler in strict SDKs (Anthropic JS v0.30+,
	// anthropic-py >=0.40), which then chokes on the non-JSON
	// `[DONE]` payload and silently aborts mid-render. Pre-fix this
	// was the root cause of Claude Code rendering an empty assistant
	// message on /v1/messages even though every upstream event had
	// arrived correctly.
	EmitOpenAIDone bool
}

func (c *LiveConfig) withDefaults() LiveConfig {
	out := *c
	if out.FirstInspectChars <= 0 {
		out.FirstInspectChars = defaultFirstInspectChars
	}
	if out.ReinspectStepChars <= 0 {
		out.ReinspectStepChars = defaultReinspectStepChars
	}
	if out.MaxBufferSize <= 0 {
		out.MaxBufferSize = defaultMaxStreamBufferSize
	}
	if out.ChannelSize <= 0 {
		out.ChannelSize = defaultEventChannelSize
	}
	return out
}

// StreamHookContext carries request-level metadata into the streaming
// compliance pipeline so that checkpoint HookInputs can be constructed
// without a full transaction context.
type StreamHookContext struct {
	RequestID      string // x-nexus-request-id for traceability
	IngressType    string
	Path           string
	Method         string
	Model          string
	SourceIP       string
	ProviderRegion string

	// OnCheckpoint is optional — invoked after each checkpoint with the full
	// compliance pipeline result (AI Gateway audit path). The live path is
	// audit-only (B1): OnCheckpoint stamps the audit tag but the live pipeline
	// never blocks or rewrites the wire, so there is no in-stream rewrite hook.
	OnCheckpoint func(*hookcore.CompliancePipelineResult)
}

// StreamHookRunner runs response-stage hooks at streaming checkpoints. A nil
// result is treated as Approve. Return the same aggregate shape as
// compliance.Pipeline.Execute.
type StreamHookRunner func(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult

// TransformChunk converts a provider SSE data payload to OpenAI format.
// Returns nil to skip the chunk.
type TransformChunk func(data []byte) ([]byte, error)

// PreHookCallback is the type alias for shared/policy/hooks/core.PreHookCallback.
// Single source of truth across all three ingress services
// (agent / compliance-proxy / ai-gateway) for "stamp Normalized before
// hooks see the input" — when this package is upgraded with new fields
// or contract refinements, hookcore.PreHookCallback evolves and both
// shared/streaming + this package's alias track automatically.
//
// Fires at every checkpoint BEFORE the hook runner. Receives the
// cumulative raw SSE wire bytes seen since stream start so each call
// re-normalizes the full accumulated payload (live mode); the caller
// is responsible for any caching/memoization if normalize cost is a
// concern.
type PreHookCallback = hookcore.PreHookCallback

// LivePipeline processes an SSE stream with checkpoint-based compliance.
type LivePipeline struct {
	config    LiveConfig
	hookRun   StreamHookRunner
	transform TransformChunk
	preHook   PreHookCallback
	logger    *slog.Logger
}

// NewLivePipeline creates a live streaming pipeline.
func NewLivePipeline(config LiveConfig, hookRun StreamHookRunner, transform TransformChunk, logger *slog.Logger) *LivePipeline {
	return &LivePipeline{
		config:    config.withDefaults(),
		hookRun:   hookRun,
		transform: transform,
		logger:    logger,
	}
}

// WithPreHook installs a callback that fires at every checkpoint
// before the hook runner. See PreHookCallback godoc. Returns the
// pipeline for chaining.
func (lp *LivePipeline) WithPreHook(fn PreHookCallback) *LivePipeline {
	lp.preHook = fn
	return lp
}

// Process reads SSE events from upstream, applies checkpoint compliance,
// and writes approved events to the client. Returns true if stream was blocked.
func (lp *LivePipeline) Process(
	ctx context.Context,
	upstream io.Reader,
	client http.ResponseWriter,
	hookCtx *StreamHookContext,
) (blocked bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type chunk struct {
		eventType string // SSE event: field from upstream (Anthropic typed events)
		data      string // transformed SSE data payload
		rawData   string // original data
	}

	eventCh := make(chan chunk, lp.config.ChannelSize)
	var wg sync.WaitGroup

	// When a PreHook callback is installed, tee upstream into a
	// goroutine-safe accumulator so checkpoint hook input can stamp
	// Registry-normalized payload. Without this hooks see flat-text
	// fallback (PayloadFromTextSegments). Mirrors the pattern in
	// shared/transport/streaming/live.go for cross-service consistency.
	var rawAcc *sharedstreaming.LockedByteBuffer
	upstreamForReader := upstream
	if lp.preHook != nil {
		rawAcc = &sharedstreaming.LockedByteBuffer{}
		upstreamForReader = io.TeeReader(upstream, rawAcc)
	}

	// --- Reader goroutine: parse upstream SSE ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(eventCh)

		parser := format.NewParser(upstreamForReader)
		defer parser.Release()
		for {
			if ctx.Err() != nil {
				return
			}
			evt, err := parser.Next()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					lp.logger.Error("SSE read error", "error", err)
				}
				return
			}
			if evt.Done {
				return
			}

			// Transform chunk through provider adapter.
			transformed := evt.Data
			if lp.transform != nil {
				out, err := lp.transform([]byte(evt.Data))
				if err != nil {
					lp.logger.Warn("chunk transform error", "error", err)
					continue
				}
				if out == nil {
					continue // skip (e.g. Anthropic ping)
				}
				transformed = string(out)
			}

			select {
			case eventCh <- chunk{eventType: evt.Type, data: transformed, rawData: evt.Data}:
			case <-ctx.Done():
				return
			}
		}
	}()

	// --- Main goroutine: compliance + write ---
	flusher, canFlush := client.(http.Flusher)

	var (
		// accBuf accumulates the canonical text seen so far — the audit-scan
		// source. strings.Builder amortizes growth (the prior `accumulated +=
		// delta` reallocated the whole transcript each chunk, O(n²) on the hot
		// path); .String() is a zero-copy snapshot.
		accBuf      strings.Builder
		totalBytes  int
		nextInspect = lp.config.FirstInspectChars
	)

	// runCheckpoint runs ONE audit-only response-stage scan over the content seen
	// so far. AUDIT-ONLY (B1): the live path carries only non-enforcing traffic — a
	// block scope routes to buffer and a redact scope to Model A upfront in
	// stream_shape — so an enforcing decision never reaches here. The checkpoint
	// fires OnCheckpoint for the audit tag (decision / reason / tags) and the
	// off-path redacted storage copy, but NEVER blocks or rewrites the already-
	// delivered wire.
	runCheckpoint := func() {
		if lp.hookRun == nil {
			return // no response-stage hooks bound: nothing to scan or stamp.
		}
		input := &hookcore.HookInput{
			RequestID:      hookCtx.RequestID,
			Stage:          "response",
			Normalized:     hookcore.PayloadFromTextSegments([]string{accBuf.String()}),
			IngressType:    hookCtx.IngressType,
			Path:           hookCtx.Path,
			Method:         hookCtx.Method,
			Model:          hookCtx.Model,
			SourceIP:       hookCtx.SourceIP,
			ProviderRegion: hookCtx.ProviderRegion,
		}

		// Let caller swap in a Registry-normalized payload so hooks see structured
		// chat content (model/tool_calls/reasoning) instead of the flat-text
		// fallback above. Receives the cumulative raw SSE wire bytes seen so far.
		if lp.preHook != nil && rawAcc != nil {
			lp.preHook(rawAcc.Snapshot(), input)
		}

		res := lp.hookRun(ctx, input)
		if hookCtx != nil && hookCtx.OnCheckpoint != nil {
			hookCtx.OnCheckpoint(res)
		}
	}

	// scanForAudit is false when no response-stage hooks are bound (nil runner).
	// In that case the per-event canonical-text accumulation (a per-chunk JSON
	// parse) and the audit checkpoints are pure no-op work — the checkpoint would
	// resolve BuildPipeline("response") to nil and stamp a synthetic Approve every
	// window. Skipping mirrors the shared LivePipeline's `l.pipeline == nil` guard
	// and the non-stream rule-free path; rec.ResponseAction already defaults to
	// ActionApprove upstream so the captured body still persists (only the
	// synthetic response_hook_decision="APPROVE" is dropped, matching non-stream).
	scanForAudit := lp.hookRun != nil

	// writeEvent delivers ONE chunk: it enforces the buffer cap, writes the SSE
	// frame (WITHOUT flushing — the caller coalesces flushes across a burst),
	// accumulates the canonical text, and runs the byte-window audit checkpoint.
	// It returns true when the buffer cap overflowed, in which case it has already
	// emitted the error frame, flushed, cancelled, closed the upstream, and set
	// blocked — the caller must stop.
	writeEvent := func(ch chunk) (overflow bool) {
		totalBytes += len(ch.rawData)
		if totalBytes > lp.config.MaxBufferSize {
			lp.logger.Error("stream buffer exceeded", "bytes", totalBytes)
			// Audit the content accumulated so far before aborting: the
			// mandatory EOF checkpoint below is skipped on this blocked path, so
			// without this scan the widening cadence would leave the tail since
			// the last intermediate checkpoint unaudited. The buffer is already
			// in hand — one O(n) scan, not a per-chunk cost.
			runCheckpoint()
			// best-effort: error notification to client; we cancel below regardless.
			// Flush BEFORE cancel — without the flush, the error frame stays
			// in the kernel buffer and the client sees a silent disconnect
			// instead of the size-overflow signal. The compliance-block path
			// at "blocked by compliance policy" above flushes for the same
			// reason; this path was missing the same call.
			_ = format.WriteError(client, "stream buffer exceeded maximum size")
			if canFlush {
				flusher.Flush()
			}
			cancel()
			// Same wedge as the shared
			// LivePipeline — cancel doesn't unblock a slow upstream
			// blocked inside format.Parser.Next. Best-effort close to
			// unblock the reader so wg.Wait() can return.
			sharedstreaming.CloseUpstreamOnExit(upstream)
			blocked = true
			return true
		}

		// AUDIT-ONLY (B1): deliver every chunk in real time — delivery is never
		// gated on the compliance checkpoint (a block scope routes to buffer and a
		// redact scope to Model A upfront, so live carries only non-enforcing
		// traffic). Accumulate the canonical text so the periodic + final
		// checkpoints scan it for the audit tag and the off-path redacted copy.
		_ = format.WriteTypedEvent(client, ch.eventType, ch.data)

		// Skip the audit-scan bookkeeping entirely when no response hooks are
		// bound: the per-chunk ExtractDeltaText JSON parse + accumulation exist
		// only to feed the checkpoint scan.
		if scanForAudit {
			accBuf.WriteString(format.ExtractDeltaText(ch.data))
			// Observe-only audit checkpoint on a widening byte cadence; it does
			// NOT gate delivery. Each checkpoint re-normalizes the FULL accumulated
			// stream (the pre-hook parses cumulative wire bytes, not just the new
			// delta), so a fixed byte-step makes the total re-normalization work
			// grow with the square of the response length. Growing the step
			// proportionally to the transcript caps the checkpoint count so the
			// total work stays linear; short streams keep the fine ReinspectStepChars
			// cadence (the proportional term overtakes only past ~8x the step), and
			// the mandatory EOF checkpoint below is always the authoritative full
			// scan, so coarser intermediate spacing never changes the final result.
			if accBuf.Len() >= nextInspect {
				runCheckpoint()
				step := lp.config.ReinspectStepChars
				if grow := accBuf.Len() / 8; grow > step {
					step = grow
				}
				nextInspect = accBuf.Len() + step
			}
		}
		return false
	}

	for ch := range eventCh {
		if writeEvent(ch) {
			break
		}

		// Opportunistic drain-then-flush: write any events ALREADY queued without
		// a per-event flush, then issue ONE flush for the whole burst. This
		// coalesces the flush syscall under load (the dominant cost on the live
		// path) while adding zero latency — a lone event finds the channel empty on
		// the first drain iteration and flushes immediately, so TTFT is unchanged.
		// The comma-ok receive is load-bearing: once the reader closes eventCh a
		// bare receive would spin returning zero-value chunks forever.
		draining := true
		for draining {
			select {
			case ch2, ok := <-eventCh:
				if !ok {
					// Reader closed the channel; stop draining. The outer range
					// exits on its next receive.
					draining = false
					break
				}
				if writeEvent(ch2) {
					draining = false
				}
			default:
				// Channel momentarily empty — flush the burst delivered so far.
				draining = false
			}
		}

		if canFlush {
			flusher.Flush()
		}
		if blocked {
			break
		}
	}

	// Mandatory final checkpoint — the authoritative audit scan of the FULL
	// response, always run at EOF (never gated behind a content-length threshold)
	// so a stream shorter than the first inspect window is still scanned for the
	// audit tag and the off-path redacted storage copy. Skipped when no response
	// hooks are bound (scanForAudit false) — there is nothing to scan or stamp.
	if !blocked && scanForAudit {
		runCheckpoint()
	}

	if !blocked && lp.config.EmitOpenAIDone {
		// best-effort: client may have already disconnected; nothing to
		// recover. The terminator only fires for OpenAI-shape ingress
		// clients — see LiveConfig.EmitOpenAIDone for why Anthropic /
		// Gemini ingress must NOT receive it.
		_ = format.WriteDone(client)
		if canFlush {
			flusher.Flush()
		}
	}

	// Drain eventCh so reader goroutine doesn't block.
	for range eventCh {
	}
	wg.Wait()

	return blocked
}
