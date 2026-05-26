package voyage

import (
	"io"
	"log/slog"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// voyageStreamDecoder rejects streaming: Voyage AI's /v1/embeddings endpoint
// returns synchronous JSON responses only (no SSE / streaming).
type voyageStreamDecoder struct {
	log *slog.Logger
}

func newStreamDecoder(log *slog.Logger) *voyageStreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &voyageStreamDecoder{log: log}
}

func (d *voyageStreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	if body != nil {
		_ = body.Close()
	}
	return nil, &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeEndpointUnsupported,
		Type:    "voyage_stream_unsupported",
		Message: "voyage: embeddings endpoint does not support streaming responses",
	}
}
