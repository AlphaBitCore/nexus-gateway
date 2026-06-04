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
	FirstInspectChars  int  // chars before first checkpoint (default 400)
	ReinspectStepChars int  // chars between subsequent checkpoints (default 128)
	MaxBufferSize      int  // max total buffer in bytes (default 8MB)
	ChannelSize        int  // internal event channel buffer (default 64); mirrors shared/transport/streaming.LiveConfig.ChannelSize (PR #24 / O5)
	HoldBack           bool // hold assistant deltas until first checkpoint

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
	// compliance pipeline result (AI Gateway audit path).
	OnCheckpoint func(*hookcore.CompliancePipelineResult)

	// OnStreamRewrite is optional — invoked when Modify rewrote the held-back
	// SSE buffer before the first client-visible flush (written slot count).
	OnStreamRewrite func(written int)
}

// StreamHookRunner runs response-stage hooks at streaming checkpoints. A nil
// result is treated as Approve. Return the same aggregate shape as
// compliance.Pipeline.Execute.
type StreamHookRunner func(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult

// TransformChunk converts a provider SSE data payload to OpenAI format.
// Returns nil to skip the chunk.
type TransformChunk func(data []byte) ([]byte, error)

// PreHookCallback is the type alias for shared/policy/hooks/core.PreHookCallback
// (#93). Single source of truth across all three ingress services
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

	// #91 — when a PreHook callback is installed, tee upstream into a
	// goroutine-safe accumulator so checkpoint hook input can stamp
	// Registry-normalized payload. Without this hooks see flat-text
	// fallback (PayloadFromTextSegments). Mirrors the #90 pattern in
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

	type pendingEvent struct {
		eventType string
		data      string
	}
	var (
		accumulated string // all text so far
		pendingText string // text since last checkpoint
		pending     []pendingEvent
		totalBytes  int
		released    bool
		nextInspect = lp.config.FirstInspectChars
	)

	flushPending := func() {
		for _, ev := range pending {
			// best-effort: if the client disconnected the next read will
			// surface ctx cancellation and tear down the stream.
			_ = format.WriteTypedEvent(client, ev.eventType, ev.data)
		}
		if canFlush {
			flusher.Flush()
		}
		pending = nil
		pendingText = ""
		released = true
	}

	runCheckpoint := func() bool {
		input := &hookcore.HookInput{
			RequestID:      hookCtx.RequestID,
			Stage:          "response",
			Normalized:     hookcore.PayloadFromTextSegments([]string{accumulated}),
			IngressType:    hookCtx.IngressType,
			Path:           hookCtx.Path,
			Method:         hookCtx.Method,
			Model:          hookCtx.Model,
			SourceIP:       hookCtx.SourceIP,
			ProviderRegion: hookCtx.ProviderRegion,
		}

		// #91 — let caller swap in a Registry-normalized payload so
		// hooks see structured chat content (model/tool_calls/reasoning)
		// instead of the flat-text fallback above. Receives the
		// cumulative raw SSE wire bytes seen so far. Mirrors the same
		// hook in shared/transport/streaming.LivePipeline at #90.
		if lp.preHook != nil && rawAcc != nil {
			lp.preHook(rawAcc.Snapshot(), input)
		}

		res := lp.hookRun(ctx, input)
		if hookCtx != nil && hookCtx.OnCheckpoint != nil {
			hookCtx.OnCheckpoint(res)
		}
		if res == nil {
			flushPending()
			return false
		}

		switch res.Decision {
		case hookcore.RejectHard:
			// best-effort: block-write may fail if the client already gave up;
			// we cancel ctx below either way.
			_ = format.WriteError(client, "blocked by compliance policy")
			if canFlush {
				flusher.Flush()
			}
			cancel()
			// PR #24 follow-up S1-code: cancel doesn't unblock a slow
			// upstream.Read; close upstream so the reader goroutine
			// exits and wg.Wait() can return promptly.
			sharedstreaming.CloseUpstreamOnExit(upstream)
			return true // blocked

		case hookcore.BlockSoft:
			// Flag but continue.
			flushPending()
			return false

		case hookcore.Modify:
			applied := false
			if len(res.ModifiedContent) > 0 && !released && len(pending) > 0 {
				repl := joinModifiedAssistantText(res.ModifiedContent)
				if repl != "" {
					line, err := format.OpenAIStreamDeltaPayload(repl)
					if err != nil {
						lp.logger.Error("stream modify marshal failed", "error", err)
					} else {
						// Modify path is OpenAI-shaped (delta envelope), so
						// emit it as an event-less SSE frame.
						pending = []pendingEvent{{eventType: "", data: line}}
						accumulated = repl
						pendingText = repl
						applied = true
						if hookCtx != nil && hookCtx.OnStreamRewrite != nil {
							hookCtx.OnStreamRewrite(1)
						}
					}
				}
			}
			if !applied {
				lp.logger.Warn("streaming response Modify skipped rewrite",
					"released", released,
					"pendingChunks", len(pending),
					"reason", res.Reason,
				)
			}
			flushPending()
			return false

		default:
			flushPending()
			return false
		}
	}

	for ch := range eventCh {
		delta := format.ExtractDeltaText(ch.data)
		totalBytes += len(ch.rawData)

		if totalBytes > lp.config.MaxBufferSize {
			lp.logger.Error("stream buffer exceeded", "bytes", totalBytes)
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
			// PR #24 follow-up S1-code: same wedge as the shared
			// LivePipeline — cancel doesn't unblock a slow upstream
			// blocked inside format.Parser.Next. Best-effort close to
			// unblock the reader so wg.Wait() can return.
			sharedstreaming.CloseUpstreamOnExit(upstream)
			blocked = true
			break
		}

		accumulated += delta
		pendingText += delta
		pending = append(pending, pendingEvent{eventType: ch.eventType, data: ch.data})

		if len(accumulated) >= nextInspect {
			if runCheckpoint() {
				blocked = true
				break
			}
			nextInspect = len(accumulated) + lp.config.ReinspectStepChars
		} else if released || !lp.config.HoldBack {
			// Already released or no hold-back — write immediately.
			for _, ev := range pending {
				// best-effort: same disconnect-tolerance as flushPending above.
				_ = format.WriteTypedEvent(client, ev.eventType, ev.data)
			}
			if canFlush {
				flusher.Flush()
			}
			pending = nil
			pendingText = ""
		}
	}

	// Final checkpoint for any remaining content.
	if !blocked && len(pendingText) > 0 {
		if runCheckpoint() {
			blocked = true
		}
	} else if !blocked && len(pending) > 0 {
		flushPending()
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

func joinModifiedAssistantText(blocks []hookcore.ContentBlock) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}
