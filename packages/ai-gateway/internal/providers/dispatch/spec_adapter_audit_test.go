package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// decodeAdapter builds a specAdapter whose transport returns a fixed 200
// JSON body and whose codec decode returns the supplied canonical body.
func decodeAdapter(canonical []byte, do func(context.Context, *http.Request) (*http.Response, error)) Adapter {
	spec := AdapterSpec{
		Format:    FormatGemini,
		Transport: &fakeTransport{do: do},
		SchemaCodec: schemaCodecFunc{
			encFn: func(_ typology.WireShape, body []byte, _ CallTarget) ([]byte, error) { return body, nil },
			decFn: func(_ typology.WireShape, _ []byte, _ string) (DecodeResult, error) {
				return DecodeResult{CanonicalBody: canonical}, nil
			},
		},
		StreamDecoder:   &fakeStreamDecoder{},
		ErrorNormalizer: &fakeErrorNormalizer{},
	}
	return NewSpecAdapter(spec, slog.Default())
}

func okJSON() func(context.Context, *http.Request) (*http.Response, error) {
	return func(_ context.Context, _ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}
}

// TestExecute_EmbeddingsModelBackfill_StampsRequestedModel covers F-0217:
// an embeddings decoder that leaves "model" empty (Gemini/Vertex wire has no
// model field) must have it back-filled with the resolved ProviderModelID so
// OpenAI SDK callers see the model they asked for.
func TestExecute_EmbeddingsModelBackfill_StampsRequestedModel(t *testing.T) {
	adapter := decodeAdapter([]byte(`{"object":"list","data":[],"model":""}`), okJSON())
	resp, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeGeminiEmbedContent,
		BodyFormat: FormatGemini,
		Body:       []byte(`{"content":{"parts":[{"text":"hi"}]}}`),
		Target:     CallTarget{ProviderModelID: "text-embedding-004"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := gjson.GetBytes(resp.Body, "model").Str; got != "text-embedding-004" {
		t.Errorf("model = %q, want back-filled %q; body=%s", got, "text-embedding-004", resp.Body)
	}
}

// TestExecute_EmbeddingsModelBackfill_PreservesDecoderModel ensures the
// back-fill never overwrites a model the decoder already stamped (F-0217).
func TestExecute_EmbeddingsModelBackfill_PreservesDecoderModel(t *testing.T) {
	adapter := decodeAdapter([]byte(`{"object":"list","data":[],"model":"voyage-3"}`), okJSON())
	resp, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeVoyageEmbeddings,
		BodyFormat: FormatGemini,
		Body:       []byte(`{"input":"hi"}`),
		Target:     CallTarget{ProviderModelID: "should-not-win"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := gjson.GetBytes(resp.Body, "model").Str; got != "voyage-3" {
		t.Errorf("model = %q, want decoder-stamped %q", got, "voyage-3")
	}
}

// TestExecute_ChatResponse_NoEmbeddingsModelBackfill ensures the back-fill is
// scoped to embeddings — a chat response with an empty model is left untouched
// (F-0217: the rule keys on EndpointKindEmbeddings, not all responses).
func TestExecute_ChatResponse_NoEmbeddingsModelBackfill(t *testing.T) {
	adapter := decodeAdapter([]byte(`{"object":"chat.completion","model":""}`), okJSON())
	resp, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeGeminiGenerateContent,
		BodyFormat: FormatGemini,
		Body:       []byte(`{"contents":[]}`),
		Target:     CallTarget{ProviderModelID: "gemini-2.5-flash"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := gjson.GetBytes(resp.Body, "model").Str; got != "" {
		t.Errorf("chat model must not be back-filled, got %q", got)
	}
}

// withTimeoutBudget temporarily sets the upstream timeout budget and restores
// the previous config when the test ends.
func withTimeoutBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	prev := specutil.ActiveConfig()
	specutil.Configure(specutil.HTTPConfig{Timeout: budget})
	t.Cleanup(func() { specutil.Configure(prev) })
}

// TestExecute_NonStream_AppliesContextDeadline covers F-0054: a non-streaming
// upstream call must carry the configured per-request deadline so a connected
// upstream cannot run unbounded.
func TestExecute_NonStream_AppliesContextDeadline(t *testing.T) {
	withTimeoutBudget(t, 30*time.Second)
	var sawDeadline bool
	adapter := decodeAdapter([]byte(`{}`), func(ctx context.Context, _ *http.Request) (*http.Response, error) {
		_, sawDeadline = ctx.Deadline()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})
	if _, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"messages":[]}`),
		Target:     CallTarget{ProviderModelID: "m"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !sawDeadline {
		t.Error("non-stream upstream call must carry a context deadline (F-0054)")
	}
}

// TestExecute_Stream_NoBudgetDeadline covers F-0054: a streaming upstream call
// must NOT inherit the per-frame budget deadline — the body is read lazily
// after Execute returns, so a deadline here would abort a healthy stream.
func TestExecute_Stream_NoBudgetDeadline(t *testing.T) {
	withTimeoutBudget(t, 30*time.Second)
	var sawDeadline bool
	spec := AdapterSpec{
		Format: FormatOpenAI,
		Transport: &fakeTransport{do: func(ctx context.Context, _ *http.Request) (*http.Response, error) {
			_, sawDeadline = ctx.Deadline()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}},
		SchemaCodec:     &fakeCodec{},
		StreamDecoder:   &fakeStreamDecoder{},
		ErrorNormalizer: &fakeErrorNormalizer{},
	}
	adapter := NewSpecAdapter(spec, slog.Default())
	if _, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"messages":[]}`),
		Stream:     true,
		Target:     CallTarget{ProviderModelID: "m"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sawDeadline {
		t.Error("streaming upstream call must NOT carry the per-frame budget deadline (F-0054)")
	}
}

// TestExecute_NonStream_DeadlineExceeded_MapsToTimeout covers F-0054: when the
// configured budget elapses on a non-stream call, the gateway returns a typed
// CodeTimeout / 504 instead of a generic upstream error.
func TestExecute_NonStream_DeadlineExceeded_MapsToTimeout(t *testing.T) {
	withTimeoutBudget(t, 20*time.Millisecond)
	adapter := decodeAdapter([]byte(`{}`), func(ctx context.Context, _ *http.Request) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"messages":[]}`),
		Target:     CallTarget{ProviderModelID: "m"},
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T: %v", err, err)
	}
	if pe.Code != CodeTimeout {
		t.Errorf("Code = %q, want %q", pe.Code, CodeTimeout)
	}
	if pe.Status != http.StatusGatewayTimeout {
		t.Errorf("Status = %d, want %d", pe.Status, http.StatusGatewayTimeout)
	}
}
