// Package responses — codec_responses_status.go carries the status axis of
// the Responses-API codec: which bodies are answers at all, and how a
// Responses `status` + `incomplete_details.reason` pair corresponds to a
// chat-completions `finish_reason` in both directions.
//
// Split from codec_responses_response.go, which owns the content axis
// (walking output[] into choices[0] and back). The two change for
// different reasons — content mapping follows new output item types, the
// status mapping follows new lifecycle states — and the failure modes are
// different too: a content bug loses part of an answer, a status bug
// misreports whether there was one.
package responses

import (
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// responsesTerminalFailure reports a Responses body that did not produce a
// usable answer, as a ProviderError. Returns nil for every status that
// carries one.
//
// Two families qualify, and both would otherwise reach the caller as a
// success:
//
//	failed / errored          the upstream disowned the answer and says why
//	                          in `error`; there is no chat-completions
//	                          finish_reason that means "this failed", so
//	                          decoding can only misreport it as `stop`.
//	in_progress / queued      the response is unfinished. These are only
//	                          reachable in background mode, which this
//	                          gateway does not send; receiving one on a
//	                          synchronous call means the body is not an
//	                          answer, whatever partial output it carries.
//
// Any other status — including one OpenAI has not shipped yet — is left to
// the decoder, so a new terminal status degrades to a decoded answer rather
// than a hard failure.
func responsesTerminalFailure(raw []byte) *provcore.ProviderError {
	status := gjson.GetBytes(raw, "status").String()
	switch status {
	case "failed", "errored":
	case "in_progress", "queued":
		return &provcore.ProviderError{
			Status:  http.StatusBadGateway,
			Code:    provcore.CodeUpstreamError,
			Type:    "incomplete_response",
			Message: "upstream returned a /v1/responses body still in status " + status + " on a synchronous request",
			Raw:     raw,
		}
	default:
		return nil
	}

	pe := &provcore.ProviderError{
		Status:  http.StatusBadGateway,
		Code:    provcore.CodeUpstreamError,
		Type:    gjson.GetBytes(raw, "error.code").String(),
		Message: gjson.GetBytes(raw, "error.message").String(),
		Raw:     raw,
	}
	if pe.Type == "" {
		pe.Type = "response_" + status
	}
	if pe.Message == "" {
		pe.Message = "upstream reported /v1/responses status " + status + " with no error detail"
	}
	return pe
}

// mapResponsesStatusToFinishReason converts Responses-API status +
// incomplete_details.reason into a chat-completions finish_reason.
// Per OpenAI's documented mapping:
//
//	completed                                  → stop (or tool_calls when output had function_call items)
//	incomplete (max_output_tokens)             → length
//	incomplete (content_filter)                → content_filter
//
// The statuses with no honest finish_reason — failed, errored, in_progress,
// queued — never reach here: [responsesTerminalFailure] turns them into a
// ProviderError before the canonical body is built. The default arm is
// therefore reached only by a status OpenAI has not shipped yet, and treats
// it as a completed answer rather than inventing a failure.
func mapResponsesStatusToFinishReason(status, incompleteReason string, hadToolCalls bool) string {
	switch status {
	case "completed":
		if hadToolCalls {
			return "tool_calls"
		}
		return "stop"
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "length"
		}
	default:
		return "stop"
	}
}

// mapFinishReasonToResponsesStatus is the inverse: canonical
// finish_reason → Responses status.
func mapFinishReasonToResponsesStatus(finish string) string {
	switch finish {
	case "length", "max_tokens", "content_filter":
		return "incomplete"
	case "", "stop", "tool_calls":
		return "completed"
	default:
		return "completed"
	}
}

// mapFinishReasonToResponsesIncompleteReason maps a finish_reason that
// implies incomplete status to the Responses incomplete_details.reason
// field. Returns "" when finish_reason does not imply incomplete.
func mapFinishReasonToResponsesIncompleteReason(finish string) string {
	switch finish {
	case "length", "max_tokens":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	default:
		return ""
	}
}
