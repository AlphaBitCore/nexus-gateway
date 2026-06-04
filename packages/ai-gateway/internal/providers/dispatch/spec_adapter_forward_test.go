package dispatch

import (
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"io"
	"net/http"
	"strings"
	"testing"

	"log/slog"
)

type noopTransport struct {
	captured http.Header
}

func (n *noopTransport) BuildURL(_ CallTarget, endpoint typology.WireShape, stream bool) (string, error) {
	if stream {
		return "http://127.0.0.1/stream", nil
	}
	return "http://127.0.0.1/chat", nil
}

func (n *noopTransport) ApplyAuth(_ *http.Request, _ CallTarget) error { return nil }

func (n *noopTransport) Do(_ context.Context, r *http.Request) (*http.Response, error) {
	n.captured = r.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}

func (n *noopTransport) Probe(context.Context, CallTarget) (*ProbeResult, error) {
	return &ProbeResult{OK: true}, nil
}

type noopCodec struct{}

func (noopCodec) EncodeRequest(endpoint typology.WireShape, body []byte, _ CallTarget) (EncodeResult, error) {
	return EncodeResult{Body: body, ContentType: "application/json"}, nil
}

func (noopCodec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, _ string) (DecodeResult, error) {
	return DecodeResult{CanonicalBody: nativeBody}, nil
}

// TestSpecAdapter_ForwardsAnthropicBeta confirms that the embedded
// default forward-header allowlist (consulted when the adapter has
// no explicit *forwardheader.Resolved injected) forwards
// anthropic-beta for the Anthropic adapter type — preserving the
// historical hardcoded behavior after the lift to YAML.
func TestSpecAdapter_ForwardsAnthropicBeta(t *testing.T) {
	tr := &noopTransport{}
	spec := AdapterSpec{
		Format:          FormatAnthropic,
		Transport:       tr,
		SchemaCodec:     noopCodec{},
		StreamDecoder:   noopStream{},
		ErrorNormalizer: noopNorm{},
	}
	ad := NewSpecAdapter(spec, slog.Default())
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatAnthropic,
		Body:       []byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`),
		Target:     CallTarget{BaseURL: "http://example.invalid"},
		Headers: http.Header{
			"Content-Type":   []string{"application/json"},
			"Anthropic-Beta": []string{"prompt-caching-2024-07-31"},
		},
		Stream: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := tr.captured.Get("Anthropic-Beta")
	if v != "prompt-caching-2024-07-31" {
		t.Fatalf("Anthropic-Beta header not forwarded, got %q", v)
	}
}

type noopStream struct{}

func (noopStream) Open(io.ReadCloser, typology.WireShape) (StreamSession, error) {
	return nil, io.EOF
}

type noopNorm struct{}

func (noopNorm) Normalize(int, http.Header, []byte) *ProviderError { return nil }
