package bedrock

import (
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// errorNormalizer maps Bedrock runtime errors. Bedrock surfaces JSON
// error envelopes typed by the `__type` field, e.g.
//
//	{"__type":"ThrottlingException","message":"..."}
//	{"__type":"ValidationException","message":"..."}
//	{"__type":"ModelNotReadyException","message":"..."}
type errorNormalizer struct{}

// Normalize implements provcore.ErrorNormalizer.
func (errorNormalizer) Normalize(status int, _ http.Header, body []byte) *provcore.ProviderError {
	pe := &provcore.ProviderError{Status: status, Raw: body}
	pe.Type = gjson.GetBytes(body, "__type").String()
	if pe.Type == "" {
		pe.Type = gjson.GetBytes(body, "type").String()
	}
	pe.Message = gjson.GetBytes(body, "message").String()
	if pe.Message == "" {
		pe.Message = gjson.GetBytes(body, "Message").String()
	}
	if pe.Message == "" {
		pe.Message = http.StatusText(status)
	}
	switch pe.Type {
	case "ThrottlingException", "TooManyRequestsException":
		pe.Code = provcore.CodeRateLimited
	case "ValidationException":
		pe.Code = provcore.CodeInvalidRequest
	case "AccessDeniedException", "UnrecognizedClientException":
		pe.Code = provcore.CodeAuthFailed
	case "ModelNotReadyException", "ServiceUnavailableException", "InternalServerException":
		pe.Code = provcore.CodeUpstreamError
	case "ModelTimeoutException":
		pe.Code = provcore.CodeTimeout
	}
	if pe.Code == "" {
		switch status {
		case http.StatusBadRequest:
			pe.Code = provcore.CodeInvalidRequest
		case http.StatusUnauthorized, http.StatusForbidden:
			pe.Code = provcore.CodeAuthFailed
		case http.StatusTooManyRequests:
			pe.Code = provcore.CodeRateLimited
		case http.StatusRequestTimeout, http.StatusGatewayTimeout:
			pe.Code = provcore.CodeTimeout
		default:
			pe.Code = provcore.CodeUpstreamError
		}
	}
	return pe
}
