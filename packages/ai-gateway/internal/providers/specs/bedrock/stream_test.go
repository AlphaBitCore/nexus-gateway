package bedrock

import (
	"errors"
	"io"
	"log/slog"
	"strings"
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
	if !errors.As(err, &pe) {
		t.Fatalf("expected *provcore.ProviderError, got %#v", err)
	}
	if pe.Code != provcore.CodeEndpointUnsupported {
		t.Errorf("expected CodeEndpointUnsupported, got %q", pe.Code)
	}
	if pe.Type != "bedrock_stream_unsupported" {
		t.Errorf("expected type bedrock_stream_unsupported, got %q", pe.Type)
	}
	// Message must tell callers what to do instead — not just what is broken.
	if !strings.Contains(pe.Message, "Anthropic adapter") {
		t.Errorf("error message should mention Anthropic adapter alternative, got: %q", pe.Message)
	}
	if !strings.Contains(pe.Message, "stream=false") {
		t.Errorf("error message should mention stream=false alternative, got: %q", pe.Message)
	}
}

func TestBedrockStreamDecoder_OpenWithBody_ClosesBody(t *testing.T) {
	closed := false
	rc := &closeTracker{closed: &closed}
	d := newBedrockStreamDecoder(nil)
	_, err := d.Open(rc, typology.WireShapeBedrockConverse)
	if err == nil {
		t.Fatal("expected error")
	}
	if !closed {
		t.Error("Open must close the response body even when returning an error")
	}
}

type closeTracker struct {
	closed *bool
}

func (c *closeTracker) Read(_ []byte) (int, error) { return 0, io.EOF }
func (c *closeTracker) Close() error               { *c.closed = true; return nil }
