package streaming

import (
	"errors"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// ErrRewriteUnsupported is returned by a FrameRedactor when it cannot
// reconstruct the wire for a Modify (redact) decision — e.g. tool-call
// arguments were masked but the per-host adapter cannot put them back on
// its native wire (it is not a traffic.ToolArgMasker). It signals the
// BufferPipeline to FAIL CLOSED: the original (unredacted) frames are
// never replayed; the policy error frame is delivered instead and the
// degrade counter is bumped under reason="redact_inflight_unsupported".
//
// A FrameRedactor that wants the DISCLOSED "forward the original body,
// stamp the audit honestly" degraded path (e.g. tlsbump's
// REDACT_INFLIGHT_UNSUPPORTED behavior, matching the non-stream request
// path) must NOT return this error — it returns the original events with
// a nil error and stamps the reason code on the result itself. This
// error is reserved for the genuinely non-reconstructable wire, which is
// not safe to forward unredacted.
var ErrRewriteUnsupported = errors.New("streaming: frame redactor cannot reconstruct wire for redaction")

// FrameRedactor is the per-service seam that turns a buffered SSE
// timeline into a REDACTED one when the response pipeline returns a
// Modify decision. It is the substrate that makes buffer mode actually
// redact instead of degrading the Modify and replaying the original
// (a PII leak).
//
// Locus per the locked design: the redaction happens at the layer that
// owns the wire. The ai-gateway redacts the CANONICAL body before the
// transcoder (one impl, also the B2 Model-A escalation target) and does
// NOT install a FrameRedactor here. The tlsbump per-host path DOES
// install one — it decodes the buffered wire frames to canonical via the
// per-host adapter, redacts, then re-encodes wire (span-splice over text
// frames; tool_use / ping / role / [DONE] frames pass byte-verbatim).
//
// Contract:
//   - Returns the events to replay to the client. The supported case
//     replaces sensitive text in text frames in place and passes every
//     non-text frame byte-verbatim; the buffer replays exactly what is
//     returned, so a returned slice is the client-visible transcript.
//   - Returns ErrRewriteUnsupported when the wire is genuinely
//     non-reconstructable → the buffer fails closed (see above).
//   - Any other non-nil error is treated as fail-closed as well (no
//     original replay) — a redactor that hit an internal error must not
//     leak the unredacted stream.
//   - On ANY non-nil error return, the implementation MUST record a
//     `nexus_streaming_modify_degraded_total` root-cause label (via
//     streaming.RecordModifyDegraded) BEFORE returning — the buffer pipeline
//     deliberately does NOT bump a coarse counter on the redactor's error arm
//     (it would double-count, see buffer.go), so this metric is the only
//     operator signal that a fail-closed redaction-unsupported event occurred.
//     The production sseFrameRedactor satisfies this in redactUnsupported.
//
// The implementation MUST be re-entrant: BufferPipeline.Process can run
// concurrently for many requests, each with its own redactor instance,
// so any accumulated write-back state lives per call, never on a package
// global (mirrors the 04-DESIGN per-call writeCtx accumulator pattern).
type FrameRedactor interface {
	RedactReplay(events []*SSEEvent, result *core.CompliancePipelineResult) ([]*SSEEvent, error)
}

// reasonRedactInflightUnsupported is the modify-degraded counter reason
// for the fail-closed (non-reconstructable wire) buffer path. Distinct
// from "buffer_mode" (the legacy no-redactor degrade) so an operator can
// tell a not-wired pipeline from a genuinely-unreconstructable wire.
const reasonRedactInflightUnsupported = "redact_inflight_unsupported"
