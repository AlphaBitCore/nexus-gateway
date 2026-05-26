package proxy

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// stubTrafficAdapter is a shared test double for handler tests. It
// implements [traffic.Adapter] with minimal behavior so callers can
// assert which adapter was selected without depending on the real
// format-specific adapters in `shared/traffic/adapters`.
type stubTrafficAdapter struct {
	id string
	// extractRequest, extractResponse, and rewriteRequest let individual
	// tests override behavior without reaching for sub-types.
	extractRequest  func(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error)
	extractResponse func(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error)
	rewriteRequest  func(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error)
}

func (s *stubTrafficAdapter) ID() string                       { return s.id }
func (s *stubTrafficAdapter) Configure(_ map[string]any) error { return nil }
func (s *stubTrafficAdapter) DetectRequestMeta(_ *http.Request, _ []byte) traffic.RequestMeta {
	return traffic.RequestMeta{}
}
func (s *stubTrafficAdapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{}
}
func (s *stubTrafficAdapter) ExtractStreamChunk(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
	return traffic.NormalizedContent{}, nil
}

func (s *stubTrafficAdapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if s.extractRequest != nil {
		return s.extractRequest(ctx, body, path)
	}
	return traffic.NormalizedContent{}, nil
}

func (s *stubTrafficAdapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if s.extractResponse != nil {
		return s.extractResponse(ctx, body, path)
	}
	return traffic.NormalizedContent{}, nil
}

func (s *stubTrafficAdapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	if s.rewriteRequest != nil {
		return s.rewriteRequest(ctx, body, path, content)
	}
	return body, 0, nil
}

func (s *stubTrafficAdapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, nil
}
