package proxy

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/guardrail"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// guardrail_capture.go — payload capture for POST /v1/guardrail, under the same
// operator switch that governs every other request and response body.
//
// This endpoint used to store no body at all, on the reasoning that the
// evaluated text is the caller's sensitive material. That reasoning answers the
// wrong question. Sensitivity governs WHO may read a body, WHETHER it is masked,
// and HOW LONG it is kept — three questions this deployment already answers, with
// the operator's payload_capture switch, the pipeline's own redaction spans, and
// IAM on the traffic detail. Dropping the bytes substituted one blunt answer for
// all three, and did it only here: the identical sentence sent to
// /v1/chat/completions is stored, so the retention policy for a piece of text
// depended on which door it came in by.
//
// It also made the one record that most needs reconstruction the one record that
// could not be reconstructed. A guardrail row IS a verdict about content; an
// auditor asking what the verdict applied to got the decision, the tags and the
// coverage, and never the text. The same argument was already accepted for STT
// (see captureSTTAudio: "the transcription was auditable and the thing
// transcribed was not") — guardrail is that shape and was simply left out.

// captureGuardrailRequest records the evaluated text on the audit row, before
// the body is parsed, so a request rejected as malformed still records what the
// caller actually sent. This is where the chat path captures too (see the
// admission stage), for the same reason.
//
// The bytes are HANDED OVER, not copied — raw is the handler's own read buffer
// and nothing writes to it afterwards.
//
// Storing them is not yet decided at this point: under a redact or block verdict
// the audit writer's storage gate (redact.StorageRawBodyChecked) fail-safes these
// raw bytes to NULL and keeps only the masked copy that redactGuardrailRequest
// attaches once the pipeline has run.
func (h *Handler) captureGuardrailRequest(rec *audit.Record, raw []byte) {
	if !h.payloadCaptureConfig().StoreRequestBody || len(raw) == 0 {
		return
	}
	rec.AttachOwnedRequestBody(raw, "application/json")
}

// redactGuardrailRequest attaches the masked copy of the evaluated text, which
// is the only body the storage gate may persist under a redact or block verdict.
//
// req and result come from the same evaluation the verdict was built from, so
// the copy stored here and the redacted_content echoed to the caller are the one
// projection; they cannot drift.
func (h *Handler) redactGuardrailRequest(rec *audit.Record, req *guardrail.Request, result *hookcore.CompliancePipelineResult) {
	if len(rec.RequestBody) == 0 || result == nil {
		return
	}
	if rec.RequestAction != hookcore.ActionRedact && rec.RequestAction != hookcore.ActionBlock {
		return
	}
	rec.RequestBodyRedacted = guardrail.RedactedRequestBody(req, result.TransformSpans)
}

// captureGuardrailResponse records the verdict body on the audit row.
//
// The verdict is OUR output, not the caller's text: it carries the decision, the
// labels, and redaction spans expressed as offsets — never the matched
// substrings (see guardrail.Redaction). The only evaluated text it can carry is
// redacted_content, which is by construction already masked. So it stores under
// the plain approve disposition even when the verdict itself was a block.
func (h *Handler) captureGuardrailResponse(rec *audit.Record, body []byte) {
	if !h.payloadCaptureConfig().StoreResponseBody || len(body) == 0 {
		return
	}
	rec.ResponseBody = body
	rec.ResponseContentType = "application/json"
}
