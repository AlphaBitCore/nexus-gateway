package bedrock

import (
	"io"
	"log/slog"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// bedrockStreamDecoder permanently rejects streaming for the Bedrock adapter.
//
// AWS Bedrock InvokeModelWithResponseStream uses a proprietary AWS EventStream
// binary framing protocol (not SSE/newline-delimited JSON). This adapter speaks
// Bedrock's non-streaming InvokeModel endpoint and does not implement EventStream
// decoding. Callers that require streaming should either:
//   - Use the Anthropic adapter pointed at api.anthropic.com (native SSE streaming), or
//   - Use the non-streaming Bedrock inference path (stream=false in the request).
//
// Open always returns a typed bedrock_stream_unsupported ProviderError.
type bedrockStreamDecoder struct {
	log *slog.Logger
}

func newBedrockStreamDecoder(log *slog.Logger) *bedrockStreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &bedrockStreamDecoder{log: log}
}

func (d *bedrockStreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	if body != nil {
		_ = body.Close()
	}
	return nil, &provcore.ProviderError{
		Status: http.StatusBadRequest,
		Code:   provcore.CodeEndpointUnsupported,
		Type:   "bedrock_stream_unsupported",
		Message: "bedrock adapter does not support streaming: AWS EventStream binary framing is not implemented. " +
			"Use the Anthropic adapter for streaming, or set stream=false to use non-streaming Bedrock inference.",
	}
}
