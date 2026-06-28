package streaming

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

const (
	defaultCheckpointChars    = 500
	defaultMinCheckpointChars = 200
	defaultMaxCheckpointChars = 2000
	defaultMaxBufferSize      = 8 * 1024 * 1024 // 8 MB
	defaultChannelSize        = 64
)

// LiveConfig configures the live streaming compliance pipeline.
type LiveConfig struct {
	CheckpointChars    int // chars between checkpoints (default 500)
	MinCheckpointChars int // adaptive lower bound (default 200)
	MaxCheckpointChars int // adaptive upper bound (default 2000)
	MaxBufferSize      int // max total buffer (default 8MB)
	ChannelSize        int // internal channel buffer (default 64)
}

func (c *LiveConfig) withDefaults() LiveConfig {
	out := *c
	if out.CheckpointChars <= 0 {
		out.CheckpointChars = defaultCheckpointChars
	}
	if out.MinCheckpointChars <= 0 {
		out.MinCheckpointChars = defaultMinCheckpointChars
	}
	if out.MaxCheckpointChars <= 0 {
		out.MaxCheckpointChars = defaultMaxCheckpointChars
	}
	if out.MaxBufferSize <= 0 {
		out.MaxBufferSize = defaultMaxBufferSize
	}
	if out.ChannelSize <= 0 {
		out.ChannelSize = defaultChannelSize
	}
	return out
}

// PipelineExecutor abstracts the compliance pipeline for testability.
type PipelineExecutor interface {
	Execute(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult
}

// approvedChunk is a batch of events approved by a checkpoint evaluation.
type approvedChunk struct {
	events []*SSEEvent
	err    error  // non-nil signals the writer should emit an error and stop
	reason string // human-readable rejection reason, if any
}

// LivePipeline processes an SSE stream with checkpoint-based compliance core.
type LivePipeline struct {
	config   LiveConfig
	pipeline PipelineExecutor
	logger   *slog.Logger
	usage    UsageAccumulator // optional; fed every parsed frame when non-nil
	// captureBuf accumulates the raw bytes streamed to the client, capped
	// at the WithBodyCapture(maxBytes) boundary so the audit emitter can
	// persist a prefix of the SSE response body. nil when capture is off.
	captureBuf *CappedBuffer
	// preHook runs at every checkpoint before pipeline.Execute. See
	// PreHookCallback godoc (shared with BufferPipeline). Receives the
	// raw SSE wire bytes accumulated since stream start (not since last
	// checkpoint) so each call can re-normalize the full cumulative
	// payload against the Registry chain.
	preHook PreHookCallback
}

// NewLivePipeline creates a live streaming compliance pipeline.
func NewLivePipeline(config LiveConfig, pipeline PipelineExecutor, logger *slog.Logger) *LivePipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &LivePipeline{
		config:   config.withDefaults(),
		pipeline: pipeline,
		logger:   logger,
	}
}

// WithUsageAccumulator attaches a usage accumulator that is fed every parsed
// SSE frame during Process. Caller retains ownership and must call
// acc.Finalize(ctx) after Process returns to read the UsageMeta.
func (l *LivePipeline) WithUsageAccumulator(acc UsageAccumulator) *LivePipeline {
	l.usage = acc
	return l
}

// WithPreHook installs a callback that fires at every checkpoint before
// pipeline.Execute, with the cumulative raw SSE wire bytes seen so far.
// Lets the caller stamp checkpointInput.Normalized (and audit-info
// ResponseNormalized) with a Registry-normalized payload so hook
// pipelines see structured chat content rather than the flat-text
// fallback buildCheckpointInput would otherwise produce.
//
// Cost: each checkpoint re-runs normalize on the cumulative body — for
// long streams that's quadratic in body size if normalize itself is
// O(n). Acceptable in practice because (a) checkpoints fire every
// ~500 chars by default so the per-call body is bounded by
// MaxBufferSize anyway, (b) normalize.Registry is structured around
// O(n) parse + adapter dispatch, (c) Registry tier 1 adapters (claude-
// web, chatgpt-web, anthropic, openai-chat) are byte-parser-fast.
func (l *LivePipeline) WithPreHook(fn PreHookCallback) *LivePipeline {
	l.preHook = fn
	return l
}

// WithBodyCapture enables capturing up to maxBytes of the bytes streamed to
// the client so the audit pipeline can persist the SSE response body the
// same way it persists non-stream bodies. Pass 0 (or never call) to leave
// capture disabled. After Process returns, retrieve the captured prefix via
// CapturedBytes() and the overflow flag via CapturedTruncated().
func (l *LivePipeline) WithBodyCapture(maxBytes int) *LivePipeline {
	l.captureBuf = NewCappedBuffer(maxBytes)
	return l
}

// CapturedBytes returns the bytes streamed to the client, capped at the
// WithBodyCapture limit. Returns nil when capture was not enabled.
func (l *LivePipeline) CapturedBytes() []byte {
	return l.captureBuf.Bytes()
}

// CapturedTruncated reports whether the captured body hit the per-call cap.
// Audit consumers stamp this on the SpillRef-equivalent so the UI can
// render a "(truncated)" indicator.
func (l *LivePipeline) CapturedTruncated() bool {
	return l.captureBuf.Truncated()
}

// Process reads SSE events from upstream, applies checkpoint-based compliance
// hooks, and writes approved events to the client writer.
// Returns the aggregated compliance result.
func (l *LivePipeline) Process(
	ctx context.Context,
	upstream io.Reader,
	client io.Writer,
	baseInput *core.HookInput,
) (*core.CompliancePipelineResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Resolve the http.Flusher interface BEFORE wrapping the writer in a
	// MultiWriter — MultiWriter doesn't carry through interface satisfactions,
	// so SSE clients would otherwise see no events until the connection closed.
	flusher, canFlush := client.(http.Flusher)

	// Tee writes into the capture buffer when WithBodyCapture is on. The
	// MultiWriter preserves the existing client write semantics — every
	// approved SSEEvent's bytes go to the client first, then to the capped
	// capture buffer (which never errors so the client write is never
	// aborted by capture).
	if l.captureBuf != nil {
		client = io.MultiWriter(client, l.captureBuf)
	}

	// When a PreHook callback is installed, tee the upstream reader
	// into a thread-safe accumulator so the compliance goroutine can read
	// a cumulative raw-bytes snapshot at every checkpoint and feed it to
	// the Registry. Without this, checkpoint hooks only see the flat-text
	// fallback from buildCheckpointInput. The reader writes to the
	// accumulator inline (no extra goroutine); the compliance goroutine
	// reads a snapshot via .Snapshot() which locks briefly + copies.
	var rawAcc *LockedByteBuffer
	upstreamForReader := upstream
	if l.preHook != nil {
		rawAcc = &LockedByteBuffer{}
		upstreamForReader = io.TeeReader(upstream, rawAcc)
	}

	eventChan := make(chan *SSEEvent, l.config.ChannelSize)
	approvedChan := make(chan approvedChunk, l.config.ChannelSize)

	var (
		wg          sync.WaitGroup
		readerErr   error
		writerErr   error
		finalResult *core.CompliancePipelineResult
	)

	// --- Reader goroutine ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(eventChan)

		parser := NewSSEParserWithLogger(upstreamForReader, l.logger)
		for {
			if ctx.Err() != nil {
				return
			}
			evt, err := parser.Next()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readerErr = err
					l.logger.Error("SSE reader error", "error", err)
				}
				return
			}
			if l.usage != nil {
				l.usage.Feed(evt)
			}
			select {
			case eventChan <- evt:
			case <-ctx.Done():
				return
			}
			if evt.Done {
				return
			}
		}
	}()

	// --- Compliance goroutine ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(approvedChan)

		var (
			pendingEvents []*SSEEvent
			// accumulatedAll is the full text accumulated so far (fed to every
			// checkpoint). pendingText is the text since the last checkpoint.
			// Both use strings.Builder so appending each SSE delta is amortized
			// O(1); naive `s += delta` in this per-event loop is O(n²) over the
			// length of a long stream. pendingLen tracks the
			// builder's current length so the checkpoint threshold check stays a
			// cheap int compare instead of len(pendingText.String()).
			accumulatedAll strings.Builder
			pendingText    strings.Builder
			pendingLen     int
			totalBytes     int
			allResults     []core.HookResult
		)

		flushCheckpoint := func() core.Decision {
			if len(pendingEvents) == 0 && pendingLen == 0 {
				return core.Approve
			}

			checkpointInput := buildCheckpointInput(baseInput, accumulatedAll.String())

			// Let caller swap in a Registry-normalized payload so
			// hooks see structured chat content (model name, tool_calls,
			// reasoning segments, etc.) instead of the flat-text fallback.
			// Receives the cumulative raw SSE wire bytes seen so far so
			// each checkpoint re-normalizes the full accumulated payload
			// — matches the "hooks operate on what user has seen so far"
			// chunked_async semantic the user binding specifies.
			if l.preHook != nil && rawAcc != nil {
				l.preHook(rawAcc.Snapshot(), checkpointInput)
			}

			result := l.pipeline.Execute(ctx, checkpointInput)
			if result == nil {
				// Pipeline returned nil — treat as approve.
				return core.Approve
			}

			allResults = append(allResults, result.HookResults...)

			switch result.Decision {
			case core.RejectHard:
				finalResult = &core.CompliancePipelineResult{
					Decision:    core.RejectHard,
					Reason:      result.Reason,
					ReasonCode:  result.ReasonCode,
					HookResults: allResults,
				}
				select {
				case approvedChan <- approvedChunk{err: fmt.Errorf("blocked by policy"), reason: result.Reason}:
				case <-ctx.Done():
				}
				cancel()
				// cancel alone doesn't unblock a reader sitting in
				// upstream.Read — close the upstream so the reader
				// goroutine exits and wg.Wait() can return (same wedge
				// as the writer-error and overflow branches).
				CloseUpstreamOnExit(upstream)
				return core.RejectHard

			default:
				// Approve or Abstain — flush pending events.
				select {
				case approvedChan <- approvedChunk{events: pendingEvents}:
				case <-ctx.Done():
					return core.Approve
				}
				pendingEvents = nil
				pendingText.Reset()
				pendingLen = 0
				return core.Approve
			}
		}

		for evt := range eventChan {
			if ctx.Err() != nil {
				return
			}

			deltaText := extractDeltaText(evt)
			totalBytes += len(evt.Data)

			if totalBytes > l.config.MaxBufferSize {
				l.logger.Error("live pipeline: max buffer size exceeded", "bytes", totalBytes)
				select {
				case approvedChan <- approvedChunk{err: fmt.Errorf("stream buffer exceeded maximum size")}:
				case <-ctx.Done():
				}
				cancel()
				// cancel doesn't unblock a slow upstream.Read — close
				// the upstream so the reader goroutine exits and
				// wg.Wait() can return (same wedge as the writer-error
				// path).
				CloseUpstreamOnExit(upstream)
				return
			}

			pendingEvents = append(pendingEvents, evt)
			pendingText.WriteString(deltaText)
			accumulatedAll.WriteString(deltaText)
			pendingLen += len(deltaText)

			// Check if we've reached the checkpoint threshold.
			if pendingLen >= l.config.CheckpointChars {
				decision := flushCheckpoint()
				if decision == core.RejectHard {
					return
				}
			}
		}

		// Final checkpoint: flush remaining accumulated text.
		flushCheckpoint()

		// Build final aggregate result if not already set by a rejection.
		if finalResult == nil {
			finalResult = &core.CompliancePipelineResult{
				Decision:    core.Approve,
				HookResults: allResults,
			}
		}
	}()

	// --- Writer goroutine (runs on current goroutine) ---
	// flusher / canFlush were resolved above against the original client
	// before MultiWriter wrapping (see comment near captureBuf init).
	for chunk := range approvedChan {
		if chunk.err != nil {
			writerErr = writeErrorAndDone(client)
			if canFlush {
				flusher.Flush()
			}
			// Drain remaining items so the compliance goroutine does not block.
			for range approvedChan {
			}
			break
		}
		writeOK := true
		for _, evt := range chunk.events {
			if err := WriteSSEEvent(client, evt); err != nil {
				writerErr = err
				cancel()
				// cancel alone does NOT unblock a reader goroutine
				// sitting inside upstream.Read. If
				// upstream is a slow / hung connection, the reader stays
				// blocked until upstream actually delivers bytes or the
				// caller's outer defer Close() runs — but Process can't
				// return until wg.Wait() does, and wg.Wait() can't return
				// until the reader exits. CloseUpstreamOnExit calls
				// upstream.(io.Closer).Close synchronously to unblock
				// the reader's parser.Next; caller's outer defer Close
				// is idempotent on http.Body.
				CloseUpstreamOnExit(upstream)
				writeOK = false
				break
			}
		}
		if !writeOK {
			break
		}
		if canFlush {
			flusher.Flush()
		}
	}

	// Wait for reader and compliance goroutines to finish.
	wg.Wait()

	if writerErr != nil {
		return finalResult, writerErr
	}
	if readerErr != nil {
		return finalResult, readerErr
	}
	return finalResult, nil
}

// writeErrorAndDone writes a JSON error event and a [DONE] marker.
func writeErrorAndDone(w io.Writer) error {
	errEvt := &SSEEvent{
		Event: "message",
		Data:  `{"error": "blocked by policy"}`,
		Retry: -1,
	}
	if err := WriteSSEEvent(w, errEvt); err != nil {
		return err
	}
	doneEvt := &SSEEvent{
		Event: "message",
		Data:  "[DONE]",
		Done:  true,
		Retry: -1,
	}
	return WriteSSEEvent(w, doneEvt)
}

// buildCheckpointInput constructs a HookInput for a streaming checkpoint evaluation.
// It copies the network context from baseInput and sets the accumulated text as
// the single content block so hooks see the full content accumulated so far.
func buildCheckpointInput(base *core.HookInput, accumulatedText string) *core.HookInput {
	input := &core.HookInput{
		Stage:       base.Stage,
		SourceIP:    base.SourceIP,
		TargetHost:  base.TargetHost,
		Method:      base.Method,
		Path:        base.Path,
		IngressType: base.IngressType,
		ContentType: base.ContentType,
		BodySize:    base.BodySize,
		Normalized:  core.PayloadFromTextSegments([]string{accumulatedText}),
	}
	return input
}

// extractDeltaText attempts to extract the delta content from an SSE event's
// data field. For OpenAI-compatible streaming responses, the data is JSON with
// choices[0].delta.content. Falls back to the raw data if parsing fails.
func extractDeltaText(evt *SSEEvent) string {
	if evt.Done {
		return ""
	}
	data := evt.Data
	if data == "" {
		return ""
	}

	// Try to parse as OpenAI streaming chunk.
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err == nil {
		if len(chunk.Choices) > 0 {
			return chunk.Choices[0].Delta.Content
		}
		return ""
	}

	// Fallback: return raw data as text.
	return data
}

// CloseUpstreamOnExit unblocks a reader goroutine that's parked
// inside upstream.Read. Called from the writer-error / overflow
// branches where ctx cancel alone isn't enough — slow HTTP responses
// don't observe ctx cancellation until the next read, and a
// completely-silent upstream never observes it.
//
// Synchronous on purpose: Close on http.Body / *strings.Reader is
// fast, and the calling goroutine is already on the exit path
// (writer error → break out of for-loop). Making this async via a
// goroutine creates a race where Process can return before Close
// has actually fired, which defeats the wedge-prevention guarantee
// (Process completes, the next call uses the same upstream which is
// still open, …).
//
// Best-effort: if upstream isn't an io.Closer (e.g. a strings.Reader
// in tests) the function is a no-op. Close errors are intentionally
// ignored — we're already on the exit path.
func CloseUpstreamOnExit(upstream io.Reader) {
	if closer, ok := upstream.(io.Closer); ok {
		_ = closer.Close()
	}
}
