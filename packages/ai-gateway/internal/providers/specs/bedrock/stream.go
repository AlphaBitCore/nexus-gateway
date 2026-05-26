package bedrock

import (
	"io"
	"log/slog"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// bedrockStreamDecoder rejects streaming: Bedrock InvokeModelWithResponseStream
// uses AWS event-stream framing, not SSE.
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
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeEndpointUnsupported,
		Type:    "bedrock_stream_unsupported",
		Message: "bedrock: InvokeModelWithResponseStream is not supported for this adapter",
	}
}
