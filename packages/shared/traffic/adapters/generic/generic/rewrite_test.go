package generic

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_AlwaysUnsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{"messages":[{"content":"x"}]}`), "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"y"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}

func TestRewriteResponseBody_AlwaysUnsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"choices":[{"message":{"content":"x"}}]}`), "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"y"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}
