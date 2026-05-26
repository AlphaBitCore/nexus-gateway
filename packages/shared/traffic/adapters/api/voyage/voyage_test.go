package voyage_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/voyage"
)

func newAdapter() *voyage.Adapter { return &voyage.Adapter{} }

func TestAdapter_ID(t *testing.T) {
	a := newAdapter()
	if a.ID() != "voyage" {
		t.Fatalf("ID() = %q, want voyage", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := newAdapter()
	if err := a.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

func TestExtractRequest_StringInput(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"model":"voyage-3","input":"hello voyage"}`)
	got, err := a.ExtractRequest(context.Background(), body, "")
	if err != nil {
		t.Fatalf("ExtractRequest: %v", err)
	}
	if len(got.Segments) != 1 || got.Segments[0] != "hello voyage" {
		t.Errorf("segments = %v, want [hello voyage]", got.Segments)
	}
	if got.Metadata["model"] != "voyage-3" {
		t.Errorf("metadata model = %q", got.Metadata["model"])
	}
}

func TestExtractRequest_ArrayInput(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"model":"voyage-3","input":["alpha","beta","gamma"]}`)
	got, err := a.ExtractRequest(context.Background(), body, "")
	if err != nil {
		t.Fatalf("ExtractRequest: %v", err)
	}
	if len(got.Segments) != 3 {
		t.Fatalf("segments len = %d, want 3", len(got.Segments))
	}
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if got.Segments[i] != want {
			t.Errorf("segments[%d] = %q, want %q", i, got.Segments[i], want)
		}
	}
}

func TestExtractRequest_InputTypeMetadata(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"model":"voyage-3","input":"text","input_type":"query"}`)
	got, err := a.ExtractRequest(context.Background(), body, "")
	if err != nil {
		t.Fatalf("ExtractRequest: %v", err)
	}
	if got.Metadata["input_type"] != "query" {
		t.Errorf("input_type metadata = %q, want query", got.Metadata["input_type"])
	}
}

func TestExtractRequest_MissingInput_ReturnsUnknownSchema(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"model":"voyage-3"}`)
	_, err := a.ExtractRequest(context.Background(), body, "")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractRequest_MalformedJSON_ReturnsMalformed(t *testing.T) {
	a := newAdapter()
	_, err := a.ExtractRequest(context.Background(), []byte(`{not json`), "")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractRequest_NumericInput_ReturnsUnknownSchema(t *testing.T) {
	a := newAdapter()
	// Numeric (token-array) inputs are not the Voyage wire shape.
	body := []byte(`{"model":"voyage-3","input":42}`)
	_, err := a.ExtractRequest(context.Background(), body, "")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("expected ErrUnknownSchema for numeric input, got %v", err)
	}
}

func TestExtractResponse_HappyPath(t *testing.T) {
	a := newAdapter()
	body := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],
		"model":"voyage-3",
		"usage":{"total_tokens":5}
	}`)
	got, err := a.ExtractResponse(context.Background(), body, "")
	if err != nil {
		t.Fatalf("ExtractResponse: %v", err)
	}
	// Embedding vectors are not stored in segments per SDD §T2.3.
	if len(got.Segments) != 0 {
		t.Errorf("segments must be empty on response side, got %v", got.Segments)
	}
	if got.Metadata["model"] != "voyage-3" {
		t.Errorf("metadata model = %q", got.Metadata["model"])
	}
}

func TestExtractResponse_MissingDataArray_ReturnsUnknownSchema(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"object":"list","model":"voyage-3"}`)
	_, err := a.ExtractResponse(context.Background(), body, "")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractResponse_MalformedJSON_ReturnsMalformed(t *testing.T) {
	a := newAdapter()
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractStreamChunk_ReturnsEmpty(t *testing.T) {
	a := newAdapter()
	got, err := a.ExtractStreamChunk(context.Background(), []byte(`{"data":"x"}`), "")
	if err != nil {
		t.Fatalf("ExtractStreamChunk: %v", err)
	}
	if len(got.Segments) != 0 {
		t.Errorf("streaming must return empty segments, got %v", got.Segments)
	}
}

func TestDetectRequestMeta_BearerToken(t *testing.T) {
	a := newAdapter()
	r, _ := http.NewRequest(http.MethodPost, "https://api.voyageai.com/v1/embeddings", nil)
	r.Header.Set("Authorization", "Bearer pa-testkey1234")
	body := []byte(`{"model":"voyage-3","input":"hi"}`)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "voyage" {
		t.Errorf("Provider = %q, want voyage", meta.Provider)
	}
	if meta.ApiKeyClass != "voyage-bearer" {
		t.Errorf("ApiKeyClass = %q, want voyage-bearer", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint == "" {
		t.Error("ApiKeyFingerprint must be non-empty")
	}
	if meta.Model != "voyage-3" {
		t.Errorf("Model = %q, want voyage-3", meta.Model)
	}
}

func TestDetectRequestMeta_NoAuth(t *testing.T) {
	a := newAdapter()
	meta := a.DetectRequestMeta(nil, []byte(`{"model":"voyage-3","input":"hi"}`))
	if meta.Provider != "voyage" {
		t.Errorf("Provider = %q, want voyage", meta.Provider)
	}
	if meta.ApiKeyFingerprint != "" {
		t.Errorf("ApiKeyFingerprint must be empty with no auth header, got %q", meta.ApiKeyFingerprint)
	}
}

func TestDetectResponseUsage_HappyPath(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"object":"list","data":[],"model":"voyage-3","usage":{"total_tokens":42}}`)
	u := a.DetectResponseUsage(nil, body)
	if u.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %v, want OK", u.Status)
	}
	if u.PromptTokens == nil || *u.PromptTokens != 42 {
		t.Errorf("PromptTokens = %v, want 42", u.PromptTokens)
	}
}

func TestDetectResponseUsage_EmptyBody(t *testing.T) {
	a := newAdapter()
	u := a.DetectResponseUsage(nil, nil)
	if u.Status != traffic.UsageStatusNoBody {
		t.Errorf("status = %v, want NoBody", u.Status)
	}
}

func TestDetectResponseUsage_MalformedJSON(t *testing.T) {
	a := newAdapter()
	u := a.DetectResponseUsage(nil, []byte(`{bad`))
	if u.Status != traffic.UsageStatusParseFailed {
		t.Errorf("status = %v, want ParseFailed", u.Status)
	}
}

func TestDetectResponseUsage_MissingUsage(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"object":"list","data":[],"model":"voyage-3"}`)
	u := a.DetectResponseUsage(nil, body)
	if u.Status != traffic.UsageStatusParseFailed {
		t.Errorf("status = %v, want ParseFailed (no usage block)", u.Status)
	}
}

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"model":"voyage-3","input":"hi"}`)
	out, _, err := a.RewriteRequestBody(context.Background(), body, "", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Fatalf("expected ErrRewriteUnsupported, got %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged on unsupported rewrite")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := newAdapter()
	body := []byte(`{"object":"list","data":[]}`)
	out, _, err := a.RewriteResponseBody(context.Background(), body, "", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Fatalf("expected ErrRewriteUnsupported, got %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged on unsupported rewrite")
	}
}
