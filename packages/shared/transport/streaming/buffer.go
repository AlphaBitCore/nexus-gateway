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
// contract defined in shared/policy/hooks/core.PreHookCallback.
// Re-exported here so SSE-pipeline callers can spell it without
// pulling the hooks/core import. Identical type — fully
// interchangeable with core.PreHookCallback.
//
// Wiring: sse.go's buffer-mode branch installs a callback (built
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
	// frameRedactor, when non-nil, turns a Modify decision into a real
	// redaction of the buffered timeline instead of degrading it to a
	// verbatim replay. See FrameRedactor godoc. Nil preserves the
	// backward-compatible degrade-and-replay-original behavior.
	frameRedactor FrameRedactor
	// strictFailClosed governs the no-redactor degrade posture (and mirrors the
	// per-host redactor's own posture from the caller): the compliance-proxy appliance
	// (true) fails a redaction it cannot apply CLOSED (error frame, no original replay);
	// the agent NE host-packet path (false) degrades to a disclosed replay of the
	// original. Nothing is delivered live in buffer mode, so failing closed here does
	// not endanger the host packet path; the flag only selects the no-redactor disposition.
	strictFailClosed bool
}

// WithPreHook installs a callback that runs between Phase 1 (read full
// body) and Phase 2 (run hooks). See PreHookCallback godoc for the
// contract. Nil disables the hook (default).
func (b *BufferPipeline) WithPreHook(fn PreHookCallback) *BufferPipeline {
	b.preHook = fn
	return b
}

// WithFrameRedactor installs the per-service redactor that rewrites the
// buffered SSE timeline when the response pipeline returns Modify. See
// FrameRedactor godoc for the seam contract. Nil (default) keeps the
// backward-compatible Modify-degrade behavior. Mirrors WithPreHook.
func (b *BufferPipeline) WithFrameRedactor(fr FrameRedactor) *BufferPipeline {
	b.frameRedactor = fr
	return b
}

// WithStrictFailClosed selects the no-redactor degrade posture (GAP B): true (the
// compliance-proxy appliance) fails a redaction it cannot apply CLOSED; false (the agent
// NE host-packet path, the default) degrades to a disclosed replay of the original.
// Mirrors the per-host redactor's own caller-driven posture.
func (b *BufferPipeline) WithStrictFailClosed(strict bool) *BufferPipeline {
	b.strictFailClosed = strict
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
	// Tee Phase 1 reads into rawBuf so the preHook callback can run
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

	// Invoke caller-provided pre-hook callback so the compliance
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

	// Phase 3: Replay, redact, or reject. Gate the redaction arm on CarriesRedaction()
	// (Modify OR a BlockSoft masking a co-firing redact), NOT Decision==Modify — a
	// co-firing soft-block promotes the aggregate Decision to BlockSoft while still
	// carrying the redact's spans + ModifiedContent, so keying on Modify alone would
	// fall through to the `default` replay arm and deliver the original RAW (the
	// appliance leak this fixes). A standalone soft-block (no redaction) keeps the
	// allow-with-warning replay on `default`.
	switch {
	case result.Decision == core.RejectHard:
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

	case result.CarriesRedaction():
		// Inflight redact. With a FrameRedactor wired, rewrite the buffered timeline so
		// the masked frames (not the original) reach the client. Without one, the
		// posture decides (GAP B): the compliance-proxy appliance fails CLOSED (a
		// redaction it cannot apply must never deliver the original); the agent NE path
		// degrades to a disclosed replay of the original (host-packet safety).
		if b.frameRedactor == nil {
			if b.strictFailClosed {
				b.logger.Warn("buffer mode: redaction required but no frame redactor; failing closed (appliance)",
					"requestId", baseInput.RequestID,
					"reason", result.Reason,
				)
				RecordModifyDegraded(reasonRedactInflightUnsupported)
				if err := writeErrorAndDone(client); err != nil {
					return result, fmt.Errorf("buffer pipeline: write error response: %w", err)
				}
				if flusher, ok := client.(http.Flusher); ok {
					flusher.Flush()
				}
				return result, nil
			}
			b.logger.Warn("buffer mode: redaction required but no frame redactor; degrading to replay (agent fail-open)",
				"requestId", baseInput.RequestID,
				"reason", result.Reason,
			)
			RecordModifyDegraded("buffer_mode")
			return result, b.replay(ctx, client, events)
		}
		redacted, rErr := b.frameRedactor.RedactReplay(events, result)
		if rErr != nil {
			// Non-reconstructable wire — FAIL CLOSED: the original frames are never replayed;
			// emit the policy error frame so zero unredacted content reaches the client. No
			// coarse counter here: the redactor already recorded the more-informative ROOT-CAUSE
			// label in redactUnsupported on this same fault (bumping the coarse one too
			// double-counted). ResponseBodyRedacted stays unset (stream-relay guard stores NULL).
			b.logger.Warn("buffer mode: Modify redaction unsupported; failing closed (no original replay)",
				"requestId", baseInput.RequestID,
				"reason", result.Reason,
				"error", rErr,
			)
			if err := writeErrorAndDone(client); err != nil {
				return result, fmt.Errorf("buffer pipeline: write error response: %w", err)
			}
			if flusher, ok := client.(http.Flusher); ok {
				flusher.Flush()
			}
			return result, nil
		}
		// Disposition: stamp action=redact when the mask was genuinely applied (a co-firing
		// BlockSoft that redact-delivered reads redact, not the block ceiling); skip it on the
		// agent fail-open degrade (RedactReplay relayed the original + ReasonRedactInflightUnsupported).
		if result.ReasonCode != core.ReasonRedactInflightUnsupported {
			result.Action = core.ActionRedact
		}
		return result, b.replay(ctx, client, redacted)

	default:
		// Approve or Abstain — replay all buffered events unchanged.
		return result, b.replay(ctx, client, events)
	}
}

// replay writes the given SSE events to the client, teeing into the
// capture buffer when WithBodyCapture is enabled and flushing after each
// frame for incremental delivery. Resolves the flusher BEFORE wrapping
// in MultiWriter — interface satisfactions don't pass through it.
func (b *BufferPipeline) replay(ctx context.Context, client io.Writer, events []*SSEEvent) error {
	flusher, canFlush := client.(http.Flusher)
	writer := client
	if b.captureBuf != nil {
		writer = io.MultiWriter(client, b.captureBuf)
	}
	for _, evt := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := WriteSSEEvent(writer, evt); err != nil {
			return fmt.Errorf("buffer pipeline: write event: %w", err)
		}
		if canFlush {
			flusher.Flush()
		}
	}
	return nil
}
