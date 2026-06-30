package tlsbump

// sse_modela.go — the wire substrate that drives the shared streaming Model-A
// engine (shared/transport/streaming/modela) over raw SSE frames for the tlsbump
// transparent proxy (agent + compliance-proxy). The engine owns the prescan-gated
// tail-hold algorithm; this adapter supplies the wire seams: parsing frames off the
// upstream, extracting per-frame visible text via the matched adapter, delivering
// frames in real time, confirming via the response pipeline, and escalating to a
// buffer-the-remainder redaction on the frame timeline.
//
// Fail posture is fail-OPEN (the agent NE host-packet path must never fail closed):
// the frame redactor relays the original frames + stamps REDACT_INFLIGHT_UNSUPPORTED
// when the masked wire cannot be soundly reconstructed (see sseFrameRedactor). A
// hard-BLOCK decision is an admin-intended enforcement (not a machinery error), so
// it still writes an in-band error frame — fail-open governs redaction faults, not
// deliberate blocks.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/modela"
)

// overrideStreamingModeForScope applies the scope-derived routing override (mirror
// ai-gateway stream_shape.go §B2). An enforcing response scope overrides the admin
// streaming mode because the live path is audit-only and cannot enforce: MayBlock
// forces "buffer" (zero-leak hard block); MayRedact forces "buffer" unless the flow
// is chunked_async ("live") with a per-host adapter, where it arms Model A
// ("modela") — prescan-gated streaming + escalate-to-buffer redaction. A redact
// under raw "passthrough" cannot be applied on the wire, so it buffers. Non-enforcing
// scopes pass through unchanged.
func overrideStreamingModeForScope(mode string, mayBlock, mayRedact, hasAdapter bool) string {
	switch {
	case mayBlock:
		return "buffer"
	case mayRedact:
		switch mode {
		case "live":
			if hasAdapter {
				return "modela"
			}
			return "buffer"
		case "passthrough":
			return "buffer"
		}
	}
	return mode
}

// scopeRouteSSEMode builds the response probe once and applies the scope-derived
// streaming-mode override. Returns the (possibly overridden) mode and the probe
// pipeline (non-nil only when it can be reused as the Model A prescan/confirm
// pipeline). A nil respInput, an unbuildable fail-closed probe (non-strict caller),
// or no response hooks leaves the live/passthrough flow routed conservatively.
func scopeRouteSSEMode(bo *bumpOptions, mode string, respInput *core.HookInput, audCtx *requestAuditCtx, logger *slog.Logger) (string, *compliance.Pipeline) {
	if respInput == nil {
		return mode, nil
	}
	probe, pErr := bo.policyResolver.BuildPipeline(
		"response", "COMPLIANCE_PROXY",
		"", nil,
		bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks,
		bo.strictFailClosed,
		logger,
	)
	switch {
	case pErr != nil:
		// A fail-closed response hook is unbuildable. Strict (appliance) callers are
		// already refused by the SSE-entry guard (clean 451); a non-strict (agent)
		// caller forces BUFFER so enforcing traffic never streams uninspected on the
		// audit-only live path.
		if mode == "live" || mode == "passthrough" {
			return "buffer", nil
		}
		return mode, nil
	case probe != nil:
		hasAdapter := audCtx != nil && audCtx.adapter != nil
		return overrideStreamingModeForScope(mode, probe.MayBlock(), probe.MayRedact(), hasAdapter), probe
	}
	return mode, nil
}

// runSSEModelA drives the wire Model-A path: it constructs the wire substrate over
// the SSE body and runs the shared engine, then emits the audit row with the
// response-stage compliance result, the captured (best-effort) wire body, and the
// finalized usage. probe and audCtx.adapter are non-nil (the routing guarantees it).
func runSSEModelA(
	ctx context.Context,
	w http.ResponseWriter,
	resp *http.Response,
	audCtx *requestAuditCtx,
	respInput *core.HookInput,
	auditInfo *compliance.AuditInfo,
	bo *bumpOptions,
	logger *slog.Logger,
	requestStart time.Time,
	probe *compliance.Pipeline,
	acc streaming.UsageAccumulator,
	captureMax int,
) {
	probe.SetAllowModify(true)
	probe.SetClearSoftOnApprove(true)

	var captureBuf *streaming.CappedBuffer
	var dest io.Writer = w
	if captureMax > 0 {
		captureBuf = streaming.NewCappedBuffer(captureMax)
		dest = io.MultiWriter(w, captureBuf)
	}
	flusher, canFlush := w.(http.Flusher)
	sseAdapter := audCtx.adapter
	ssePath := ""
	if respInput != nil {
		ssePath = respInput.Path
	}
	maxBuf := bo.streamingConfig.MaxBufferSize
	if maxBuf <= 0 {
		maxBuf = modelaDefaultMaxBufferBytes
	}
	sub := &sseWireSubstrate{
		ctx:      ctx,
		parser:   streaming.NewSSEParserWithLogger(resp.Body, logger),
		client:   dest,
		flusher:  flusher,
		canFlush: canFlush,
		codec:    adapterWireCodec{ctx: ctx, adapter: sseAdapter, path: ssePath},
		redactor: newSSEFrameRedactor(ctx, sseAdapter, ssePath, bo.strictFailClosed, logger),
		pipeline: probe,
		base:     respInput,
		acc:      acc,
		maxBuf:   maxBuf,
		logger:   logger,
	}
	maxPattern := deriveModelAMaxPattern(probe)
	// Config-time operator signal (#16): tlsbump leaves TailWindowBytes at the engine
	// default, so compare the derived bound against that default. Off the per-byte path.
	modela.WarnStreamingCoverageGap(logger, maxPattern, modela.DefaultTailWindowBytes)
	if err := modela.Run(ctx, sub, modela.Config{MaxBufferBytes: maxBuf, MaxPatternBytes: maxPattern}); err != nil {
		target := ""
		if respInput != nil {
			target = respInput.TargetHost
		}
		logger.Error("SSE Model-A pipeline error",
			"target", target,
			"error", err,
			"cancel_cause", cancelCause(ctx),
			"duration_ms", int(time.Since(requestStart).Milliseconds()),
		)
	}
	// Storage parity (T3): persist the wire capture only on a non-enforcing outcome —
	// it is then the benign, fully-delivered original. On a redaction/block escalation
	// the wire capture holds the real-time prefix raw (a sub-window value is fully
	// masked; a value larger than the tail window already delivered a bounded raw
	// prefix to the client — the disclosed best-effort WIRE surface). The durable,
	// queryable copy is held to a STRICTER standard than the ephemeral wire: an
	// enforcing outcome never persists a raw prefix.
	//
	// This is defense-in-depth over the canonical gate: emitAudit → StorageRawBody
	// already drops the body to NULL unless the response ACTION is approve. The guard
	// here keys on the DECISION (via modelaResultEnforcing → ActionFromDecision) — the
	// same judgment the engine's escalate gate uses — so a Decision/Action divergence
	// (a redact Decision masked behind an approve-shaped Action) can never persist the
	// raw capture. (Storing an off-path FULLY-redacted authoritative copy instead of
	// NULL — a complete record strictly stronger than the wire — is tracked
	// enhancement work; NULL is the safe floor.)
	var capturedBytes []byte
	if captureBuf != nil && !modelaResultEnforcing(sub.result) {
		capturedBytes = captureBuf.Bytes()
	}
	emitAudit(logger, audCtx, respInput, auditInfo, bo, sub.result, resp.StatusCode, requestStart, finalizeUsage(ctx, acc), capturedBytes)
}

// modelaResultEnforcing reports whether the Model A outcome blocked or redacted —
// the cases where the best-effort wire capture must NOT be persisted (it may carry a
// raw over-window prefix). A nil/approve result is non-enforcing.
func modelaResultEnforcing(res *core.CompliancePipelineResult) bool {
	if res == nil {
		return false
	}
	act := core.ActionFromDecision(res.Decision)
	return act == core.ActionBlock || act == core.ActionRedact
}

// modelaDefaultMaxBufferBytes mirrors the shared engine's MaxBufferBytes default
// (8 MB) so the escalation drain ceiling agrees with the main-loop ceiling when the
// admin streaming policy leaves MaxBufferSize unset.
const modelaDefaultMaxBufferBytes = 8 * 1024 * 1024

// wireUnit is the engine's unit for the wire path: one parsed SSE frame plus its
// visible text, extracted ONCE in Next so the per-frame prescan/window seams never
// re-parse the frame.
type wireUnit struct {
	evt  *streaming.SSEEvent
	text string
}

// sseWireSubstrate adapts a raw SSE stream to the shared Model-A engine. It holds
// the parser (source + escalation drain), the client writer (+ flusher), the
// per-host text codec + frame redactor, the response pipeline (prescan + confirm +
// escalation re-eval), and the per-frame audit accumulators.
type sseWireSubstrate struct {
	ctx      context.Context
	parser   *streaming.SSEParser
	client   io.Writer
	flusher  http.Flusher
	canFlush bool

	codec    streaming.WireTextCodec
	redactor *sseFrameRedactor // nil when no adapter → redaction degrades (fail-open)
	pipeline *compliance.Pipeline
	maxBuf   int // escalation drain ceiling; <=0 disables the cap

	base *core.HookInput
	acc  streaming.UsageAccumulator

	logger *slog.Logger

	// result is the authoritative response-stage compliance result the emit site
	// records: the escalation re-eval outcome on a confirmed/pressure escalation, the
	// last false-positive confirm otherwise, or an Approve on a clean stream.
	result *core.CompliancePipelineResult
}

var _ modela.Substrate[wireUnit] = (*sseWireSubstrate)(nil)

// Next parses the next SSE frame, feeds the usage accumulator, and extracts the
// frame's visible text once. io.EOF ends the stream; any other error is terminal.
func (s *sseWireSubstrate) Next(_ context.Context) (wireUnit, error) {
	evt, err := s.parser.Next()
	if err != nil {
		return wireUnit{}, err
	}
	if s.acc != nil {
		s.acc.Feed(evt)
	}
	text, _ := s.codec.ChunkText(evt.Data)
	return wireUnit{evt: evt, text: text}, nil
}

// AppendRedactableText appends the frame's visible text (extracted in Next) for the
// prescan/confirm. Non-text frames (tool_use / ping / [DONE] / reasoning-only)
// contributed empty text and add nothing.
func (s *sseWireSubstrate) AppendRedactableText(dst []byte, u wireUnit) []byte {
	return append(dst, u.text...)
}

// UnitBytes is the frame's transport size (held-bytes ceiling budget).
func (s *sseWireSubstrate) UnitBytes(u wireUnit) int { return len(u.evt.Data) }

// ContentBytes is the frame's redactable-content size (tail-window budget) — the
// extracted visible text, never the framing, so non-text frames do not evict
// redactable content from the window.
func (s *sseWireSubstrate) ContentBytes(u wireUnit) int { return len(u.text) }

// IsDone reports the SSE [DONE] terminator frame.
func (s *sseWireSubstrate) IsDone(u wireUnit) bool { return u.evt.Done }

// Deliver writes one held frame to the client in real time. A write error is
// terminal; the engine stops and the emit site records the partial outcome.
func (s *sseWireSubstrate) Deliver(_ context.Context, u wireUnit) error {
	if err := s.writeFrame(u.evt); err != nil {
		return err
	}
	return nil
}

// DeliverTerminal is a no-op for SSE: the terminator ([DONE]) is itself a frame
// delivered through Deliver as the last held unit, so there is no separate terminal
// framing to emit.
func (s *sseWireSubstrate) DeliverTerminal(_ context.Context) error { return nil }

// Prescan is the cheap union prefilter over accumulated visible text. A nil
// pipeline (no response hooks) fails safe to "always confirm".
func (s *sseWireSubstrate) Prescan(content []byte) bool {
	if s.pipeline == nil {
		return true
	}
	return s.pipeline.MayMatchRawContent(content)
}

// Confirm runs ONE full response-stage evaluation over the accumulated visible
// text. A nil pipeline (no hooks) is treated as approve; the engine resumes.
func (s *sseWireSubstrate) Confirm(ctx context.Context, content string) *core.CompliancePipelineResult {
	if s.pipeline == nil {
		return &core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}
	}
	return s.pipeline.Execute(ctx, s.checkpointInput(content))
}

// Escalate hands the live path off to a buffer-to-end redaction over the frame
// timeline. It drains the remaining frames into the held set, RE-EVALUATES the
// remainder text on its own (so the redact spans map onto the still-held frames, not
// the already-delivered prefix — the divergence fence in SpliceTextFrames requires
// the transcript to equal the evaluated content), and delivers ONLY the redacted
// remainder. res is advisory (confirmed-hit vs memory-pressure); the re-eval is
// authoritative. Redaction-fault posture follows the caller (strictFailClosed): the
// agent NE path is fail-OPEN (an unproducible mask relays the original frames +
// stamps the degraded reason); the compliance-proxy appliance is fail-CLOSED (an
// unproducible mask emits an in-band block frame). A deliberate hard block always
// emits an in-band error frame regardless of posture.
// deriveModelAMaxPattern sizes the Model-A flush-before-deliver lookahead from the
// resolved response pipeline's longest contiguous enforceable match, floored at
// modela.DefaultMaxPatternBytes so it never drops below the proven-safe baseline even if
// the derivation under-counts. Two limits stay best-effort (the engine's disclosed
// surface, not this floor): (1) UNBOUNDED patterns (*, +, {n,}) whose match can exceed the
// tail window; (2) a derived bound at/above the tail window — the engine clamps the
// lookahead below the window. For (2) the caller surfaces a config-time operator warning
// (modela.WarnStreamingCoverageGap); the operator remediation is buffered streaming mode
// (full coverage) or narrowing the rule, NOT a tail-window knob.
func deriveModelAMaxPattern(probe *compliance.Pipeline) int {
	bound, _ := probe.MaxPatternBound()
	if bound < modela.DefaultMaxPatternBytes {
		bound = modela.DefaultMaxPatternBytes
	}
	return bound
}

func (s *sseWireSubstrate) Escalate(ctx context.Context, held []wireUnit, trigger *core.CompliancePipelineResult) error {
	// Record the escalation CAUSE as a metric dimension (NOT a persisted field): a nil
	// trigger means a memory-pressure eviction of an incomplete content unit; a non-nil
	// trigger means a confirmed enforcing hit. Lets operators tell a real policy hit apart
	// from a buffer-ceiling eviction (which signals MaxBufferBytes may need raising).
	cause := streaming.ModelAEscalationConfirmed
	if trigger == nil {
		cause = streaming.ModelAEscalationMemoryPressure
	}
	streaming.RecordModelAEscalation(cause)

	// Drain the remainder of the stream into the held set (the prefix beyond the tail
	// window is already delivered and excluded from held). The drain is capped at
	// maxBuf so an arbitrarily long post-hit tail cannot grow the buffer without
	// bound (the engine's MaxBufferBytes ceiling, mirrored on the drain per the
	// engine doc + the ai-gateway sibling). On overflow the enforcing remainder
	// cannot be buffered to redact, so it is BLOCKED with an in-band error frame —
	// never relayed raw (bounded + zero-leak, the same outcome as a hard block).
	heldBytes := 0
	for i := range held {
		heldBytes += len(held[i].evt.Data)
	}
	for len(held) == 0 || !held[len(held)-1].evt.Done {
		evt, err := s.parser.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if s.acc != nil {
			s.acc.Feed(evt)
		}
		text, _ := s.codec.ChunkText(evt.Data)
		held = append(held, wireUnit{evt: evt, text: text})
		heldBytes += len(evt.Data)
		if s.maxBuf > 0 && heldBytes > s.maxBuf {
			s.result = &core.CompliancePipelineResult{Decision: core.RejectHard, Action: core.ActionBlock, Reason: "stream exceeded maximum buffer size", ReasonCode: "stream_buffer_exceeded"}
			return s.writeErrorAndDone()
		}
	}

	events := make([]*streaming.SSEEvent, len(held))
	var remainder strings.Builder
	for i, u := range held {
		events[i] = u.evt
		remainder.WriteString(u.text)
	}

	if s.pipeline == nil {
		// No response pipeline — nothing to enforce; deliver the held frames as-is.
		s.result = &core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}
		return s.writeFrames(events)
	}

	res := s.pipeline.Execute(ctx, s.checkpointInput(remainder.String()))
	s.result = res

	switch res.Decision {
	case core.RejectHard:
		// Deliberate hard block — emit an in-band error frame (zero remainder
		// content). Fail-open governs redaction machinery faults, not admin blocks.
		return s.writeErrorAndDone()
	default:
		// Modify (redact) or any decision carrying redaction work: splice the masked
		// text into the held text frames. The per-host redactor's posture follows the
		// caller (strictFailClosed): the agent NE host-packet path is fail-OPEN — on an
		// unproducible mask it relays the original frames + stamps the degraded reason
		// and returns nil; the compliance-proxy appliance is fail-CLOSED — it returns
		// ErrRewriteUnsupported, which we surface as an in-band block frame rather than
		// relay the unredacted remainder.
		out := events
		if s.redactor != nil {
			redacted, err := s.redactor.RedactReplay(events, res)
			if err != nil {
				return s.writeErrorAndDone()
			}
			out = redacted
			// Disposition: stamp action=redact ONLY when the mask was genuinely spliced —
			// NOT the agent fail-open relay-original degrade (RedactReplay returns the
			// original frames with a nil error after stamping ReasonRedactInflightUnsupported).
			// On a real splice the masked frames WERE delivered, so the audit row reads
			// redact even under a co-firing BlockSoft ceiling; on the fail-open degrade the
			// original was relayed, so the action stays the aggregate.
			if res.ReasonCode != core.ReasonRedactInflightUnsupported {
				res.Action = core.ActionRedact
			}
		}
		return s.writeFrames(out)
	}
}

// OnConfirmApproved records a non-enforcing confirm outcome (false positive) so the
// emit site reports an evaluated-approved response.
func (s *sseWireSubstrate) OnConfirmApproved(res *core.CompliancePipelineResult) {
	s.result = res
}

// OnApproveEOF records an evaluated-approved outcome when the stream reached EOF
// without any confirm — the sound prescan cleared the whole stream.
func (s *sseWireSubstrate) OnApproveEOF() {
	s.result = &core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}
}

// OnError ends the stream on a non-EOF parser error. The agent host-packet path is
// fail-open: no synthetic error frame is forced onto the wire (the upstream fault is
// recorded for audit and the partial stream is left as delivered).
func (s *sseWireSubstrate) OnError(_ context.Context, err error) error {
	if s.logger != nil {
		s.logger.Error("SSE Model-A wire substrate upstream error", "error", err)
	}
	return err
}

// checkpointInput builds a response HookInput carrying the accumulated/visible text
// as the single content block — the same flat-text shape the per-frame codec
// reconstructs, so a Modify's ModifiedContent aligns with SpliceTextFrames' per-frame
// transcript (the divergence fence holds).
func (s *sseWireSubstrate) checkpointInput(content string) *core.HookInput {
	in := &core.HookInput{
		Stage:       "response",
		Normalized:  core.PayloadFromTextSegments([]string{content}),
		IngressType: s.base.IngressType,
		TargetHost:  s.base.TargetHost,
		Method:      s.base.Method,
		Path:        s.base.Path,
		ContentType: s.base.ContentType,
		SourceIP:    s.base.SourceIP,
	}
	return in
}

// writeFrame writes one SSE frame to the client (teed into the audit capture buffer
// when enabled) and flushes so the client sees it in real time.
func (s *sseWireSubstrate) writeFrame(evt *streaming.SSEEvent) error {
	if err := streaming.WriteSSEEvent(s.client, evt); err != nil {
		return err
	}
	if s.canFlush {
		s.flusher.Flush()
	}
	return nil
}

// writeFrames writes a sequence of frames (the redacted/approved remainder).
func (s *sseWireSubstrate) writeFrames(events []*streaming.SSEEvent) error {
	for _, evt := range events {
		if err := s.writeFrame(evt); err != nil {
			return err
		}
	}
	return nil
}

// writeErrorAndDone emits the in-band block error frame + the [DONE] terminator,
// mirroring the buffer pipeline's hard-block delivery.
func (s *sseWireSubstrate) writeErrorAndDone() error {
	if err := s.writeFrame(&streaming.SSEEvent{Event: "message", Data: `{"error": "blocked by policy"}`, Retry: -1}); err != nil {
		return err
	}
	return s.writeFrame(&streaming.SSEEvent{Event: "message", Data: "[DONE]", Done: true, Retry: -1})
}
