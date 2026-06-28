package voyage_test

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/voyage"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// NewSpec — constructor + wiring.

func TestNewSpec_Format(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	if spec.Format != provcore.FormatVoyage {
		t.Errorf("Format=%v want FormatVoyage", spec.Format)
	}
}

func TestNewSpec_AllComponentsWired(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	if spec.Transport == nil {
		t.Error("Transport must be wired")
	}
	if spec.SchemaCodec == nil {
		t.Error("SchemaCodec must be wired")
	}
	if spec.StreamDecoder == nil {
		t.Error("StreamDecoder must be wired")
	}
	if spec.ErrorNormalizer == nil {
		t.Error("ErrorNormalizer must be wired")
	}
	if !spec.Valid() {
		t.Error("AdapterSpec.Valid() must return true")
	}
}

func TestNewSpec_RequestShapes(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	if len(spec.RequestShapes) != 1 || spec.RequestShapes[0] != typology.WireShapeVoyageEmbeddings {
		t.Errorf("RequestShapes=%v want [embeddings]", spec.RequestShapes)
	}
}

func TestNewSpec_NilLogger(t *testing.T) {
	// Must not panic; uses slog.Default() fallback.
	spec := voyage.NewSpec(nil)
	if !spec.Valid() {
		t.Error("NewSpec(nil) must produce a valid spec")
	}
}

// Transport — BuildURL.

func TestTransport_BuildURL_Embeddings(t *testing.T) {
	tr := voyage.NewTransport(slog.Default())
	got, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeVoyageEmbeddings, false)
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if got != "https://api.voyageai.com/v1/embeddings" {
		t.Errorf("default URL=%q want https://api.voyageai.com/v1/embeddings", got)
	}
}

func TestTransport_BuildURL_CustomBaseURL(t *testing.T) {
	tr := voyage.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{BaseURL: "https://custom.voyage.example.com/"},
		typology.WireShapeVoyageEmbeddings, false,
	)
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if !strings.HasPrefix(got, "https://custom.voyage.example.com/") {
		t.Errorf("custom URL=%q", got)
	}
	if strings.Contains(got, "com//") {
		t.Errorf("trailing slash not normalized: %q", got)
	}
}

func TestTransport_BuildURL_UnsupportedEndpoint(t *testing.T) {
	tr := voyage.NewTransport(slog.Default())
	_, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeOpenAIChat, false)
	if err == nil || !strings.Contains(err.Error(), "only embeddings") {
		t.Fatalf("expected embeddings-only error, got %v", err)
	}
}

func TestTransport_BuildURL_StreamIgnored(t *testing.T) {
	// Voyage embeddings does not stream — the stream=true flag must not
	// change the URL (no /invoke-with-response-stream suffix etc.).
	tr := voyage.NewTransport(slog.Default())
	url1, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeVoyageEmbeddings, false)
	if err != nil {
		t.Fatalf("BuildURL false: %v", err)
	}
	url2, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeVoyageEmbeddings, true)
	if err != nil {
		t.Fatalf("BuildURL true: %v", err)
	}
	if url1 != url2 {
		t.Errorf("stream=true changed URL: %q vs %q", url1, url2)
	}
}

// Transport — ApplyAuth.

func TestTransport_ApplyAuth_SetsBearer(t *testing.T) {
	tr := voyage.NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://api.voyageai.com/v1/embeddings", nil)
	if err := tr.ApplyAuth(r, provcore.CallTarget{APIKey: "vk-testkey"}); err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer vk-testkey" {
		t.Errorf("Authorization=%q want Bearer vk-testkey", got)
	}
}

func TestTransport_ApplyAuth_MissingKey(t *testing.T) {
	tr := voyage.NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://api.voyageai.com/v1/embeddings", nil)
	err := tr.ApplyAuth(r, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "missing API key") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

// Transport — Do (delegation to http.Client).

func TestTransport_Do_DelegatesToHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[],"model":"voyage-3","usage":{"total_tokens":2}}`)
	}))
	defer srv.Close()

	tr := voyage.NewTransport(slog.Default())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/embeddings", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer vk-test")
	resp, err := tr.Do(context.Background(), req, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d", resp.StatusCode)
	}
}

// Transport — Probe.

func TestTransport_Probe_MissingKey(t *testing.T) {
	tr := voyage.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || !strings.Contains(r.Detail, "missing API key") {
		t.Errorf("missing-key: %+v", r)
	}
}

func TestTransport_Probe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"voyage-3-lite","usage":{"total_tokens":1}}`)
	}))
	defer srv.Close()

	tr := voyage.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{
		APIKey:  "vk-test",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Errorf("expected OK probe, got %+v", r)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs=%d", r.LatencyMs)
	}
}

func TestTransport_Probe_400IsReachable(t *testing.T) {
	// Voyage 400 (bad input) means the key reached the API — Probe must
	// report OK=true.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"invalid input"}`)
	}))
	defer srv.Close()

	tr := voyage.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{APIKey: "vk-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Errorf("400 must report OK=true (reachable): %+v", r)
	}
}

func TestTransport_Probe_422IsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"detail":"model not found"}`)
	}))
	defer srv.Close()

	tr := voyage.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{APIKey: "vk-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Errorf("422 must report OK=true (reachable): %+v", r)
	}
}

func TestTransport_Probe_401IsNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"invalid api key"}`)
	}))
	defer srv.Close()

	tr := voyage.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{APIKey: "bad-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Errorf("401 must report OK=false: %+v", r)
	}
	if !strings.Contains(r.Detail, "401") {
		t.Errorf("Detail must contain HTTP status: %q", r.Detail)
	}
}

func TestTransport_Probe_TransportError(t *testing.T) {
	tr := voyage.NewTransport(slog.Default())
	// Point probe at an unreachable host.
	r, err := tr.Probe(context.Background(), provcore.CallTarget{
		APIKey:  "vk-test",
		BaseURL: "http://127.0.0.1:1", // nothing listening
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Errorf("transport error must report OK=false: %+v", r)
	}
	if r.Err == nil {
		t.Error("Err must be non-nil on transport error")
	}
}

// StreamDecoder — always rejects streaming.

func TestStreamDecoder_Open_RejectsStream(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	_, err := spec.StreamDecoder.Open(io.NopCloser(strings.NewReader("{}")), typology.WireShapeVoyageEmbeddings)
	if err == nil {
		t.Fatal("Open must reject streaming")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Code != provcore.CodeEndpointUnsupported {
		t.Fatalf("want CodeEndpointUnsupported, got %#v", err)
	}
}

func TestStreamDecoder_Open_ClosesBody(t *testing.T) {
	closed := false
	body := &trackingCloser{r: strings.NewReader("{}"), onClose: func() { closed = true }}
	spec := voyage.NewSpec(slog.Default())
	_, _ = spec.StreamDecoder.Open(body, typology.WireShapeVoyageEmbeddings)
	if !closed {
		t.Error("Open must close the body even on rejection")
	}
}

type trackingCloser struct {
	r       io.Reader
	onClose func()
}

func (tc *trackingCloser) Read(p []byte) (int, error) { return tc.r.Read(p) }
func (tc *trackingCloser) Close() error               { tc.onClose(); return nil }

// ErrorNormalizer — code matrix.

func TestErrorNormalizer_DetailField(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	pe := spec.ErrorNormalizer.Normalize(http.StatusBadRequest, http.Header{},
		[]byte(`{"detail":"bad model"}`))
	if pe.Message != "bad model" {
		t.Errorf("Message=%q want bad model", pe.Message)
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("Code=%q want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

func TestErrorNormalizer_MessageField(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	pe := spec.ErrorNormalizer.Normalize(http.StatusInternalServerError, http.Header{},
		[]byte(`{"message":"internal error"}`))
	if pe.Message != "internal error" {
		t.Errorf("Message=%q want internal error", pe.Message)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code=%q want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

func TestErrorNormalizer_FallbackToStatusText(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	pe := spec.ErrorNormalizer.Normalize(http.StatusTeapot, http.Header{}, []byte(`{}`))
	if pe.Message != http.StatusText(http.StatusTeapot) {
		t.Errorf("Message=%q want %q", pe.Message, http.StatusText(http.StatusTeapot))
	}
}

func TestErrorNormalizer_StatusMatrix(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	cases := []struct {
		status   int
		wantCode string
	}{
		{http.StatusBadRequest, provcore.CodeInvalidRequest},
		{http.StatusUnprocessableEntity, provcore.CodeInvalidRequest},
		{http.StatusUnauthorized, provcore.CodeAuthFailed},
		{http.StatusForbidden, provcore.CodeAuthFailed},
		{http.StatusTooManyRequests, provcore.CodeRateLimited},
		{http.StatusRequestTimeout, provcore.CodeTimeout},
		{http.StatusGatewayTimeout, provcore.CodeTimeout},
		{http.StatusInternalServerError, provcore.CodeUpstreamError},
		{http.StatusBadGateway, provcore.CodeUpstreamError},
	}
	for _, tc := range cases {
		pe := spec.ErrorNormalizer.Normalize(tc.status, http.Header{}, []byte(`{}`))
		if pe.Code != tc.wantCode {
			t.Errorf("status %d → code=%q want %q", tc.status, pe.Code, tc.wantCode)
		}
		if pe.Status != tc.status {
			t.Errorf("status %d → pe.Status=%d want same", tc.status, pe.Status)
		}
	}
}

func TestErrorNormalizer_RetryAfterSeconds(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	h := http.Header{}
	h.Set("retry-after", "5")
	pe := spec.ErrorNormalizer.Normalize(http.StatusTooManyRequests, h, []byte(`{}`))
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter must be populated on 429 with retry-after header")
	}
}

func TestErrorNormalizer_RetryAfterHTTPDate(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	h := http.Header{}
	// Use a time far in the future so the duration is always positive.
	h.Set("retry-after", "Sat, 01 Jan 2050 00:00:00 GMT")
	pe := spec.ErrorNormalizer.Normalize(http.StatusTooManyRequests, h, []byte(`{}`))
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter must be populated with HTTP-date retry-after header")
	}
}

func TestErrorNormalizer_RetryAfterPast(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	h := http.Header{}
	// Past date — duration should be clipped to 0, not negative.
	h.Set("retry-after", "Thu, 01 Jan 1970 00:00:00 GMT")
	pe := spec.ErrorNormalizer.Normalize(http.StatusTooManyRequests, h, []byte(`{}`))
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter must be non-nil for past HTTP-date")
	}
	if *pe.RetryAfter < 0 {
		t.Errorf("RetryAfter must not be negative for past date: %v", *pe.RetryAfter)
	}
}

func TestErrorNormalizer_InvalidJSON(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	pe := spec.ErrorNormalizer.Normalize(http.StatusBadRequest, http.Header{}, []byte(`not-json`))
	if pe == nil {
		t.Fatal("Normalize must return non-nil even for invalid JSON")
	}
	// Should still surface the HTTP status text as message.
	if pe.Message != http.StatusText(http.StatusBadRequest) {
		t.Errorf("Message=%q want %q", pe.Message, http.StatusText(http.StatusBadRequest))
	}
}

// Codec — EncodeRequest.

func TestCodec_EncodeRequest_StringInput(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3","input":"hello world"}`)
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("wire body invalid JSON: %v", e)
	}
	if out["input"] != "hello world" {
		t.Errorf("input=%v want hello world", out["input"])
	}
	if out["model"] != "voyage-3" {
		t.Errorf("model=%v want voyage-3", out["model"])
	}
}

func TestCodec_EncodeRequest_ArrayInput(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3","input":["a","b","c"]}`)
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("wire body invalid JSON: %v", e)
	}
	arr, ok := out["input"].([]any)
	if !ok || len(arr) != 3 {
		t.Errorf("input array=%v want [a b c]", out["input"])
	}
}

func TestCodec_EncodeRequest_EmptyArrayInput(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3","input":[]}`)
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("empty array: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	arr, _ := out["input"].([]any)
	if len(arr) != 0 {
		t.Errorf("input=%v want []", out["input"])
	}
}

func TestCodec_EncodeRequest_TokenArrayRejected(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3","input":[[100,200,300]]}`)
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("token array must be rejected")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "token_array_unsupported") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestCodec_EncodeRequest_MixedTypeArrayRejected(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3","input":["text",123]}`)
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("mixed-type array must be rejected")
	}
}

func TestCodec_EncodeRequest_TargetModelOverridesBody(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3","input":"x"}`)
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body,
		provcore.CallTarget{ProviderModelID: "voyage-code-3"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["model"] != "voyage-code-3" {
		t.Errorf("model=%v want voyage-code-3", out["model"])
	}
}

func TestCodec_EncodeRequest_DimensionsForwardedAsOutputDimension(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3","input":"x","dimensions":512}`)
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	// Canonical `dimensions` → Voyage wire `output_dimension`.
	if v, ok := out["output_dimension"].(float64); !ok || v != 512 {
		t.Errorf("output_dimension=%v want 512", out["output_dimension"])
	}
}

func TestCodec_EncodeRequest_Extensions(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	// Set all voyage extensions in the canonical body (nested sjson/gjson path).
	body := []byte(`{
		"model":"voyage-3",
		"input":"hello",
		"nexus":{"ext":{"voyage":{
			"input_type":"query",
			"output_dtype":"float",
			"output_dimension":1024,
			"truncation":true
		}}}
	}`)
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest extensions: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["input_type"] != "query" {
		t.Errorf("input_type=%v want query", out["input_type"])
	}
	if out["output_dtype"] != "float" {
		t.Errorf("output_dtype=%v want float", out["output_dtype"])
	}
	if v, ok := out["output_dimension"].(float64); !ok || v != 1024 {
		t.Errorf("output_dimension=%v want 1024", out["output_dimension"])
	}
	if out["truncation"] != true {
		t.Errorf("truncation=%v want true", out["truncation"])
	}
}

func TestCodec_EncodeRequest_MissingModel(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"input":"hello"}`)
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "missing model") {
		t.Fatalf("expected missing-model error, got %v", err)
	}
}

func TestCodec_EncodeRequest_MissingInput(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	body := []byte(`{"model":"voyage-3"}`)
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing-input error, got %v", err)
	}
}

func TestCodec_EncodeRequest_InvalidInputType(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	// Input is neither string nor array.
	body := []byte(`{"model":"voyage-3","input":42}`)
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for numeric input")
	}
}

func TestCodec_EncodeRequest_EmptyBody(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, nil, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("empty body: %v", err)
	}
	// Empty body → pass through without error; content-type set.
	if res.ContentType != "application/json" {
		t.Errorf("ContentType=%q want application/json", res.ContentType)
	}
}

func TestCodec_EncodeRequest_InvalidJSON(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeVoyageEmbeddings, []byte(`not-json`), provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid-JSON error, got %v", err)
	}
}

func TestCodec_EncodeRequest_UnsupportedEndpoint(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(`{}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected unsupported-endpoint error")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Code != provcore.CodeEndpointUnsupported {
		t.Fatalf("want CodeEndpointUnsupported, got %#v", err)
	}
}

// Codec — DecodeResponse.

func TestCodec_DecodeResponse_HappyPath(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	native := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0}],
		"model":"voyage-3",
		"usage":{"total_tokens":5}
	}`)
	res, err := spec.SchemaCodec.DecodeResponse(typology.WireShapeVoyageEmbeddings, native, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	if out["object"] != "list" {
		t.Errorf("object=%v want list", out["object"])
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len=%d want 1", len(data))
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 5 {
		t.Errorf("usage.prompt_tokens=%v want 5", usage["prompt_tokens"])
	}
	if usage["total_tokens"].(float64) != 5 {
		t.Errorf("usage.total_tokens=%v want 5", usage["total_tokens"])
	}
}

// TestCodec_DecodeResponse_CountMismatch_Rejected pins the guard: when the
// request asked for N inputs but the provider returned a different number
// of vectors, the decode must fail (→ 502) rather than serve misaligned
// vectors. The request context carries the Voyage wire request body.
func TestCodec_DecodeResponse_CountMismatch_Rejected(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	// Request asked for 3 inputs; response carries only 2 embeddings.
	reqBody := []byte(`{"model":"voyage-3","input":["a","b","c"]}`)
	native := []byte(`{"object":"list","data":[
		{"object":"embedding","embedding":[0.1],"index":0},
		{"object":"embedding","embedding":[0.2],"index":1}
	],"model":"voyage-3","usage":{"total_tokens":3}}`)
	_, err := spec.SchemaCodec.DecodeResponse(typology.WireShapeVoyageEmbeddings, native, "",
		provcore.DecodeContext{RequestBody: reqBody})
	if err == nil || !strings.Contains(err.Error(), "embedding count mismatch") {
		t.Fatalf("expected count-mismatch error, got %v", err)
	}
}

// TestCodec_DecodeResponse_CountMatch_Passes is the positive arm:
// a matching count decodes cleanly with the request context present.
func TestCodec_DecodeResponse_CountMatch_Passes(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	reqBody := []byte(`{"model":"voyage-3","input":["a","b"]}`)
	native := []byte(`{"object":"list","data":[
		{"object":"embedding","embedding":[0.1],"index":0},
		{"object":"embedding","embedding":[0.2],"index":1}
	],"model":"voyage-3","usage":{"total_tokens":3}}`)
	res, err := spec.SchemaCodec.DecodeResponse(typology.WireShapeVoyageEmbeddings, native, "",
		provcore.DecodeContext{RequestBody: reqBody})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got := gjson.GetBytes(res.CanonicalBody, "data.#").Int(); got != 2 {
		t.Errorf("data count=%d want 2", got)
	}
}

func TestCodec_DecodeResponse_EmptyBody(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	res, err := spec.SchemaCodec.DecodeResponse(typology.WireShapeVoyageEmbeddings, nil, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("empty body: %v", err)
	}
	if len(res.CanonicalBody) != 0 {
		t.Errorf("expected empty canonical body, got %q", res.CanonicalBody)
	}
}

func TestCodec_DecodeResponse_InvalidJSON(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	_, err := spec.SchemaCodec.DecodeResponse(typology.WireShapeVoyageEmbeddings, []byte(`not-json`), "", provcore.DecodeContext{})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected invalid-JSON error, got %v", err)
	}
}

func TestCodec_DecodeResponse_VoyageModelStampedInExt(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	native := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.5],"index":0}],
		"model":"voyage-3-large",
		"usage":{"total_tokens":2}
	}`)
	res, err := spec.SchemaCodec.DecodeResponse(typology.WireShapeVoyageEmbeddings, native, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	// model should be in nexus.ext.voyage.model for audit consumers.
	if !strings.Contains(string(res.CanonicalBody), "voyage-3-large") {
		t.Errorf("model not stamped in canonical: %s", res.CanonicalBody)
	}
}

func TestCodec_DecodeResponse_UnsupportedEndpointPassthrough(t *testing.T) {
	spec := voyage.NewSpec(slog.Default())
	native := []byte(`{"unexpected":"payload"}`)
	res, err := spec.SchemaCodec.DecodeResponse(typology.WireShapeOpenAIChat, native, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("unexpected-endpoint passthrough: %v", err)
	}
	if string(res.CanonicalBody) != string(native) {
		t.Errorf("expected passthrough body, got %q", res.CanonicalBody)
	}
}

// End-to-end via SpecAdapter.Execute.

func TestVoyage_Execute_ReturnsEmbeddings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"object":"list",
			"data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],
			"model":"voyage-3-lite",
			"usage":{"total_tokens":3}
		}`)
	}))
	defer srv.Close()

	a := provdispatch.NewSpecAdapter(voyage.NewSpec(slog.Default()), slog.Default())
	resp, err := a.Execute(context.Background(), provcore.Request{
		WireShape:  typology.WireShapeVoyageEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"voyage-3-lite","input":"hello"}`),
		Target: provcore.CallTarget{
			ProviderModelID: "voyage-3-lite",
			BaseURL:         srv.URL,
			APIKey:          "vk-test",
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d", resp.StatusCode)
	}
}
