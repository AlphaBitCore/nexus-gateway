package bedrock

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestBedrockStreamDecoder_OpenUnsupported(t *testing.T) {
	d := newBedrockStreamDecoder(slog.Default())
	_, err := d.Open(io.NopCloser(nil), typology.WireShapeBedrockConverse)
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Code != provcore.CodeEndpointUnsupported {
		t.Fatalf("got %#v", err)
	}
}
