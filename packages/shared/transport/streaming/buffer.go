package streaming

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

const defaultBufferMaxSize = 8 * 1024 * 1024 // 8 MB

// BufferConfig configures buffer mode.
type BufferConfig struct {
	MaxBufferSize int // max bytes (default 8MB)
}

func (c *BufferConfig) withDefaults() BufferConfig {
	out := *c
	if out.MaxBufferSize <= 0 {
		out.MaxBufferSize = defaultBufferMaxSize
	}
	return out
}

// PreHookCallback is the type alias for the canonical SSE pre-hook
// contract defined in shared/policy/hooks/core.PreHookCallback (#93).
// Re-exported here so SSE-pipeline callers can spell it without
// pulling the hooks/core import. Identical type — fully
// interchangeable with core.PreHookCallback.
//
// #90 wiring: sse.go's buffer-mode branch installs a callback (built
// by shared/transport/normalize/responseprehook.Builder) that runs
// the body through Registry.Normalize and stamps both
//
//	(a) ci.Normalized — so hooks see the real claim
//	(b) auditInfo.ResponseNormalized — so the audit row carries it
//
// before BufferPipeline.Process kicks off the hook executor. Without
// this, hooks always saw a flat-text Normalized (built from
// extractDeltaText concat in buildCheckpointInput), which kept the
// admin hook ecosystem from acting on adapter-specific structure
// (model name, tool calls, reasoning segments) for buffer mode.
type PreHookCallback = core.PreHookCallback

// BufferPipeline buffers the entire SSE stream, runs hooks on the full content,
// then replays all events to the client if approved.
type BufferPipeline struct {
	config   BufferConfig
	pipeline PipelineExecutor
	logger   *slog.Logger
	usage    UsageAccumulator // optional; fed every parsed frame when non-nil
	// captureBuf accumulates the raw bytes streamed to the client, capped
	// at the WithBodyCapture(maxBytes) boundary so the audit emitter can
	// persist a prefix of the SSE response body. nil when capture is off.
	captureBuf *CappedBuffer
	// preHook runs between Phase 1 and Phase 2 with the raw buffered
	// body bytes. See PreHookCallback godoc.
	preHook PreHookCallback
}

// WithPreHook installs a callback that runs between Phase 1 (read full
// body) and Phase 2 (run hooks). See PreHookCallback godoc for the
// contract. Nil disables the hook (default).
func (b *BufferPipeline) WithPreHook(fn PreHookCallback) *BufferPipeline {
	b.preHook = fn
	return b
}

// NewBufferPipeline creates a buffer mode pipeline.
func NewBufferPipeline(config BufferConfig, pipeline PipelineExecutor, logger *slog.Logger) *BufferPipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &BufferPipeline{
		config:   config.withDefaults(),
		pipeline: pipeline,
		logger:   logger,
	}
}

// WithUsageAccumulator attaches a usage accumulator that is fed every parsed
// SSE frame during Process. Caller retains ownership and must call
// acc.Finalize(ctx) after Process returns to read the UsageMeta.
func (b *BufferPipeline) WithUsageAccumulator(acc UsageAccumulator) *BufferPipeline {
	b.usage = acc
	return b
}

// WithBodyCapture enables capturing up to maxBytes of the bytes streamed to
// the client (during the replay phase) so the audit pipeline can persist
// the SSE body alongside the non-stream capture path. After Process,
// retrieve via CapturedBytes() / CapturedTruncated().
func (b *BufferPipeline) WithBodyCapture(maxBytes int) *BufferPipeline {
	b.captureBuf = NewCappedBuffer(maxBytes)
	return b
}

// CapturedBytes returns the bytes streamed to the client, capped at the
// WithBodyCapture limit. Returns nil when capture was not enabled.
func (b *BufferPipeline) CapturedBytes() []byte {
	return b.captureBuf.Bytes()
}

// CapturedTruncated reports whether the captured body hit the per-call cap.
func (b *BufferPipeline) CapturedTruncated() bool {
	return b.captureBuf.Truncated()
}

// Process reads all SSE events from upstream, runs compliance hooks on the full
// aggregated content, and replays events to the client only if approved.
func (b *BufferPipeline) Process(
	ctx context.Context,
	upstream io.Reader,
	client io.Writer,
	baseInput *core.HookInput,
) (*core.CompliancePipelineResult, error) {
	// Tee Phase 1 reads into rawBuf so the #90 preHook callback can run
	// Registry normalize on the raw SSE wire bytes (not just the
	// extracted delta-text concat). preHook fires between Phase 1 and
	// Phase 2 so the compliance pipeline sees the rich Normalized
	// payload, not the flat-text fallback buildCheckpointInput would
	// otherwise produce.
	var rawBuf bytes.Buffer
	teedUpstream := io.TeeReader(upstream, &rawBuf)
	parser := NewSSEParserWithLogger(teedUpstream, b.logger)

	var (
		events []*SSEEvent
		// fullText accumulates the extracted delta-text across every frame.
		// strings.Builder makes each append amortized O(1); naive `s += delta`
		// in this per-event loop is O(n²) over the stream length.
		fullText  strings.Builder
		totalSize int
	)

	// Phase 1: Read and buffer all events.
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		evt, err := parser.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("buffer pipeline: read error: %w", err)
		}

		totalSize += len(evt.Data)
		if totalSize > b.config.MaxBufferSize {
			return nil, fmt.Errorf("buffer pipeline: stream exceeded maximum buffer size of %d bytes", b.config.MaxBufferSize)
		}

		if b.usage != nil {
			b.usage.Feed(evt)
		}
		events = append(events, evt)

		deltaText := extractDeltaText(evt)
		fullText.WriteString(deltaText)

		if evt.Done {
			break
		}
	}

	// Phase 2: Run compliance hooks on the full content.
	checkpointInput := buildCheckpointInput(baseInput, fullText.String())

	// #90 — invoke caller-provided pre-hook callback so the compliance
	// hook executor sees a Registry-normalized payload rather than the
	// flat-text fallback. Callback closes over the Registry + adapter +
	// content-type at the call site (sse.go's buffer branch) and stamps
	// both checkpointInput.Normalized AND auditInfo.ResponseNormalized
	// (the latter is what lands in audit_events.normalized_response).
	if b.preHook != nil {
		b.preHook(rawBuf.Bytes(), checkpointInput)
	}

	result := b.pipeline.Execute(ctx, checkpointInput)
	if result == nil {
		result = &core.CompliancePipelineResult{
			Decision: core.Approve,
		}
	}

	// #115/R3 — Modify is not supported in buffer mode. The Phase 3
	// switch below has no Modify case; the body replays unchanged. Log +
	// bump the shared counter so admin sees the silent degradation
	// (without this signal a misconfigured hook stays invisible until
	// someone notices their rewrite never took effect). The counter
	// fires from inside BufferPipeline so all three data planes
	// (ai-gateway, compliance-proxy, agent) get the same signal via a
	// single registration.
	if result.Decision == core.Modify {
		b.logger.Warn("buffer mode: Modify decision degraded to Approve (rewrite ignored)",
			"requestId", baseInput.RequestID,
			"reason", result.Reason,
		)
		RecordModifyDegraded("buffer_mode")
	}

	// Phase 3: Replay or reject.
	switch result.Decision {
	case core.RejectHard, core.BlockSoft:
		b.logger.Info("buffer pipeline: content rejected",
			"decision", result.Decision,
			"reason", result.Reason,
		)
		// Write error event to client.
		if err := writeErrorAndDone(client); err != nil {
			return result, fmt.Errorf("buffer pipeline: write error response: %w", err)
		}
		if flusher, ok := client.(http.Flusher); ok {
			flusher.Flush()
		}
		return result, nil

	default:
		// Approve or Abstain — replay all buffered events.
		// Resolve flusher BEFORE wrapping in MultiWriter (interface
		// satisfactions don't pass through MultiWriter).
		flusher, canFlush := client.(http.Flusher)
		writer := client
		if b.captureBuf != nil {
			writer = io.MultiWriter(client, b.captureBuf)
		}
		for _, evt := range events {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if err := WriteSSEEvent(writer, evt); err != nil {
				return result, fmt.Errorf("buffer pipeline: write event: %w", err)
			}
			if canFlush {
				flusher.Flush()
			}
		}
		return result, nil
	}
}
