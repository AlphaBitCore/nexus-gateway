package streaming

import (
	"context"
	"errors"
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
	CheckpointChars int // base chars between checkpoints (default 500); the cadence widens with the transcript
	// MinCheckpointChars / MaxCheckpointChars are RESERVED and currently
	// unused — no caller sets them and the checkpoint cadence is
	// deliberately uncapped (a fixed upper bound would re-introduce
	// quadratic re-normalization on very long streams). Kept for wire/field
	// compatibility; do not treat MaxCheckpointChars as a cadence ceiling.
	MinCheckpointChars int
	MaxCheckpointChars int
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
// Cost: each checkpoint re-runs normalize on the cumulative body. To keep
// the total normalize work linear in the response length rather than
// quadratic, the checkpoint cadence widens with the accumulated transcript
// (see the step calc in Process) so the checkpoint count stays sublinear;
// the mandatory final checkpoint is always the authoritative full scan.
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

// foldHookResults collapses the per-checkpoint scans of the same hook into ONE
// result: LatencyMs/LatencyUs are summed (the real scan CPU spent across the
// stream) and every other field is taken from the latest scan (the last checkpoint
// is authoritative). The chunked_async response pipeline runs once per checkpoint
// and appends each scan's results, so without this fold the same hook is emitted N
// times — observed as a "RESPONSE PIPELINE (63)" list of identical rows — and the
// response hook aggregates are N×-inflated. Keyed by stable identity
// (HookID+ImplementationID), not the volatile per-scan Order; first-seen order is
// preserved.
func foldHookResults(in []core.HookResult) []core.HookResult {
	if len(in) <= 1 {
		return in
	}
	type key struct{ hookID, implID string }
	idx := make(map[key]int, len(in))
	out := make([]core.HookResult, 0, len(in))
	for _, r := range in {
		k := key{r.HookID, r.ImplementationID}
		if i, ok := idx[k]; ok {
			r.LatencyMs += out[i].LatencyMs
			r.LatencyUs += out[i].LatencyUs
			out[i] = r // latest scan authoritative for all non-latency fields
		} else {
			idx[k] = len(out)
			out = append(out, r)
		}
	}
	return out
}

// Process reads SSE events from upstream and relays them to the client in REAL TIME
// (write-through), running observe-only compliance checkpoints on the accumulated
// content for AUDIT. It never holds back, blocks, or rewrites the wire: an enforcing
// (block/redact) scope is routed to the buffer / Model A paths upstream, so this
// path carries only non-enforcing traffic and a checkpoint's decision is recorded
// but never gates delivery. Returns the aggregated (always-Approve) result.
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

	var (
		wg        sync.WaitGroup
		readerErr error
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

	// --- Delivery + observe-only audit (current goroutine) ---
	// accumulatedAll grows with the cumulative response text (bounded by
	// MaxBufferSize); strings.Builder keeps the per-event append amortized O(1).
	// pendingLen counts chars since the last checkpoint so the cadence check stays a
	// cheap int compare.
	var (
		accumulatedAll strings.Builder
		pendingLen     int
		totalBytes     int
		allResults     []core.HookResult
		auditCapped    bool
		writerErr      error
	)

	// runCheckpoint fires the compliance pipeline OBSERVE-ONLY: it records the hook
	// results for the audit aggregate but never blocks or rewrites the wire. The
	// PreHook re-normalizes the cumulative raw bytes so hooks see structured content.
	runCheckpoint := func() {
		// Audit-only relay with no executor or no base input has nothing to record —
		// skip the checkpoint rather than deref a nil. Delivery is independent of the
		// checkpoint, so the stream still writes through. (Reached when a caller builds
		// a hookless live pipeline for usage accumulation only, e.g. AI traffic whose
		// response stage binds no hooks.)
		if l.pipeline == nil || baseInput == nil {
			return
		}
		checkpointInput := buildCheckpointInput(baseInput, accumulatedAll.String())
		if l.preHook != nil && rawAcc != nil {
			l.preHook(rawAcc.Snapshot(), checkpointInput)
		}
		if result := l.pipeline.Execute(ctx, checkpointInput); result != nil {
			allResults = append(allResults, result.HookResults...)
		}
	}

	for evt := range eventChan {
		// AUDIT-ONLY: deliver every event in real time — delivery is NEVER gated on a
		// checkpoint. A write error closes the upstream so the reader goroutine exits
		// and wg.Wait() can return (the slow-upstream wedge guard).
		if err := WriteSSEEvent(client, evt); err != nil {
			writerErr = err
			cancel()
			CloseUpstreamOnExit(upstream)
			break
		}
		if canFlush {
			flusher.Flush()
		}

		if auditCapped {
			continue
		}
		deltaText := extractDeltaText(evt)
		accumulatedAll.WriteString(deltaText)
		pendingLen += len(deltaText)
		totalBytes += len(evt.Data)
		if totalBytes > l.config.MaxBufferSize {
			// Audit-accumulation cap: stop scanning further content to bound memory,
			// but KEEP delivering — an audit-only relay must never break a
			// non-enforcing stream just because it grew past the scan budget.
			l.logger.Warn("live pipeline: audit accumulation capped at max buffer size", "bytes", totalBytes)
			auditCapped = true
			continue
		}
		// Widen the checkpoint cadence with the transcript: each checkpoint
		// re-normalizes the FULL accumulated body (the pre-hook parses cumulative
		// wire bytes), so a fixed step makes the total re-normalization work grow
		// with the square of the response length — a long stream saturates the
		// box on parsing alone. Growing the step proportionally to the accumulated
		// length keeps the checkpoint count sublinear and the total work linear.
		// Short streams keep the fixed CheckpointChars cadence; the mandatory
		// final checkpoint below always covers the trailing content, so coarser
		// intermediate spacing never changes the final audit result.
		step := l.config.CheckpointChars
		if grow := accumulatedAll.Len() / 8; grow > step {
			step = grow
		}
		if pendingLen >= step {
			runCheckpoint()
			pendingLen = 0
		}
	}

	// Mandatory final checkpoint (observe-only): scan the trailing content not yet
	// covered by a periodic checkpoint so a stream shorter than the cadence — or the
	// tail after the last checkpoint — is still audited once. Skipped when a write
	// error aborted delivery or when the last checkpoint already covered everything.
	if writerErr == nil && pendingLen > 0 {
		runCheckpoint()
	}

	// Wait for the reader goroutine to finish.
	wg.Wait()

	finalResult := &core.CompliancePipelineResult{Decision: core.Approve, HookResults: foldHookResults(allResults)}
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
