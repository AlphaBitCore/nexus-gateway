package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestExecute_DecodeStructuredProviderError_PropagatesVerbatim pins the
// dispatcher contract the cross-shape image codec depends on: a
// *ProviderError returned by SchemaCodec.DecodeResponse (a deliberate
// status the codec chose for a 2xx upstream body — e.g. the image
// codec's content-policy 400) propagates VERBATIM instead of being
// flattened into the generic 502 upstream_error wrap. The Code is
// load-bearing: CodeInvalidRequest keeps the result out of the
// executor's retry/failover set, so a provider-safety-blocked prompt is
// never re-dispatched to the next paid target.
func TestExecute_DecodeStructuredProviderError_PropagatesVerbatim(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`)),
			}, nil
		},
	}
	structured := &ProviderError{
		Status:  http.StatusBadRequest,
		Code:    CodeInvalidRequest,
		Type:    "content_policy_violation",
		Message: "blocked by provider safety",
	}
	codec := &fakeCodec{decodeErr: structured}
	ad := NewSpecAdapter(specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe != structured {
		t.Fatalf("structured decode error must propagate verbatim; got %+v", pe)
	}
	if pe.Status != http.StatusBadRequest || pe.Code != CodeInvalidRequest || pe.Type != "content_policy_violation" {
		t.Fatalf("propagated error mutated: %+v", pe)
	}
}
