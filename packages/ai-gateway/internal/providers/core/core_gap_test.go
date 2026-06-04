// core_gap_test.go fills coverage gaps in the providers/core package.
// White-box test (package core) to access unexported types and exercise
// branches not reachable from the external _test package.
package core

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestRegistry_MustRegister_success(t *testing.T) {
	r := NewRegistry()
	// MustRegister should not panic on a valid adapter.
	r.MustRegister(stubCoreAdapter{})
	a, ok := r.Get(FormatOpenAI)
	if !ok || a == nil {
		t.Fatal("MustRegister: adapter not registered")
	}
}

func TestRegistry_MustRegister_panicOnError(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubCoreAdapter{})
	defer func() {
		if recover() == nil {
			t.Error("MustRegister duplicate: expected panic")
		}
	}()
	r.MustRegister(stubCoreAdapter{}) // duplicate → panic
}

func TestRegistry_List_returnsRegisteredFormats(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubCoreAdapter{})
	list := r.List()
	found := false
	for _, f := range list {
		if f == FormatOpenAI {
			found = true
		}
	}
	if !found {
		t.Error("List: FormatOpenAI not in result")
	}
}

func TestRegistry_List_emptyRegistry(t *testing.T) {
	r := NewRegistry()
	list := r.List()
	if list == nil {
		// nil or empty slice are both acceptable; just ensure no panic.
		t.Error("List on empty registry should return a slice (not nil)")
	}
	// len 0 is fine; verify no panic.
}

func TestAdapterSpec_Valid_completeSpec_returnsTrue(t *testing.T) {
	spec := AdapterSpec{
		Format:          FormatOpenAI,
		Transport:       &stubTransport{},
		SchemaCodec:     &stubSchemaCodec{},
		StreamDecoder:   &stubStreamDecoder{},
		ErrorNormalizer: &stubErrorNormalizer{},
	}
	if !spec.Valid() {
		t.Error("fully populated AdapterSpec should be Valid()")
	}
}

func TestAdapterSpec_Valid_missingFormat_returnsFalse(t *testing.T) {
	spec := AdapterSpec{
		Transport:       &stubTransport{},
		SchemaCodec:     &stubSchemaCodec{},
		StreamDecoder:   &stubStreamDecoder{},
		ErrorNormalizer: &stubErrorNormalizer{},
	}
	if spec.Valid() {
		t.Error("AdapterSpec with invalid format should not be Valid()")
	}
}

func TestAdapterSpec_Valid_nilTransport_returnsFalse(t *testing.T) {
	spec := AdapterSpec{
		Format:          FormatOpenAI,
		SchemaCodec:     &stubSchemaCodec{},
		StreamDecoder:   &stubStreamDecoder{},
		ErrorNormalizer: &stubErrorNormalizer{},
	}
	if spec.Valid() {
		t.Error("AdapterSpec with nil Transport should not be Valid()")
	}
}

func TestFormat_IsOpenAIFamily_openAIFormats(t *testing.T) {
	// Formats that share the OpenAI wire shape.
	openAIForms := []Format{
		FormatOpenAI, FormatDeepSeek, FormatGLM, FormatAzureOpenAI,
		FormatMoonshot, FormatMistral, FormatXai, FormatGroq,
		FormatPerplexity, FormatTogether, FormatFireworks,
		FormatMiniMax, FormatHuggingFace,
	}
	for _, f := range openAIForms {
		if !f.IsOpenAIFamily() {
			t.Errorf("%s: expected IsOpenAIFamily=true", f)
		}
	}
}

func TestFormat_IsOpenAIFamily_nonOpenAIFormats(t *testing.T) {
	// Formats that do NOT share the OpenAI wire shape.
	nonOpenAI := []Format{
		FormatAnthropic, FormatBedrock, FormatGemini, FormatVertex,
		FormatCohere, FormatReplicate,
	}
	for _, f := range nonOpenAI {
		if f.IsOpenAIFamily() {
			t.Errorf("%s: expected IsOpenAIFamily=false", f)
		}
	}
}

// ExtractUsage: HuggingFace (branches not reached by consistency test)

func TestExtractUsage_huggingFace_openAICompatFormat(t *testing.T) {
	// HuggingFace uses the OpenAI wire format, so extraction should succeed.
	body := []byte(`{
		"id":"hf-1","model":"mistralai/Mistral-7B-Instruct-v0.1",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`)
	u := ExtractUsage(body, FormatHuggingFace)
	if u.PromptTokens == nil || *u.PromptTokens != 10 {
		t.Errorf("HuggingFace: PromptTokens: got %v, want 10", u.PromptTokens)
	}
}

func TestExtractUsage_miniMax_openAICompatFormat(t *testing.T) {
	body := []byte(`{
		"id":"mm-1","model":"MiniMax-Text-01",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
	}`)
	u := ExtractUsage(body, FormatMiniMax)
	if u.PromptTokens == nil || *u.PromptTokens != 20 {
		t.Errorf("MiniMax: PromptTokens: got %v, want 20", u.PromptTokens)
	}
}

func TestExtractUsage_defaultUnknownFormat_returnsZero(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":5}}`)
	u := ExtractUsage(body, Format("completely-unknown-format"))
	// Unknown format → default case → zero Usage.
	if u.PromptTokens != nil || u.CompletionTokens != nil {
		t.Errorf("unknown format: expected zero Usage, got %+v", u)
	}
}

// Stubs for AdapterSpec.Valid tests

// stubCoreAdapter implements Adapter for registry tests (reuse pattern from registry_test.go).
type stubCoreAdapter struct{}

func (stubCoreAdapter) Format() Format { return FormatOpenAI }
func (stubCoreAdapter) SupportsShape(s typology.WireShape) bool {
	return s == typology.WireShapeOpenAIChat
}
func (stubCoreAdapter) Execute(_ context.Context, _ Request) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}
func (stubCoreAdapter) Probe(_ context.Context, _ CallTarget) (*ProbeResult, error) {
	return &ProbeResult{OK: true}, nil
}
func (stubCoreAdapter) PrepareBody(req Request) ([]byte, []string, error) {
	return req.Body, nil, nil
}
func (stubCoreAdapter) ExecuteWithBody(_ context.Context, _ Request, _ []byte, _ []string) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}

type stubTransport struct{}

func (s *stubTransport) BuildURL(_ CallTarget, _ typology.WireShape, _ bool) (string, error) {
	return "https://api.example.com/v1/chat/completions", nil
}
func (s *stubTransport) ApplyAuth(_ *http.Request, _ CallTarget) error { return nil }
func (s *stubTransport) Do(_ context.Context, r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK}, nil
}
func (s *stubTransport) Probe(_ context.Context, _ CallTarget) (*ProbeResult, error) {
	return &ProbeResult{OK: true}, nil
}

type stubSchemaCodec struct{}

func (s *stubSchemaCodec) EncodeRequest(_ typology.WireShape, body []byte, _ CallTarget) (EncodeResult, error) {
	return EncodeResult{Body: body, ContentType: "application/json"}, nil
}
func (s *stubSchemaCodec) DecodeResponse(_ typology.WireShape, body []byte, _ string) (DecodeResult, error) {
	return DecodeResult{CanonicalBody: body}, nil
}

type stubStreamDecoder struct{}

func (s *stubStreamDecoder) Open(_ io.ReadCloser, _ typology.WireShape) (StreamSession, error) {
	return nil, nil
}

type stubErrorNormalizer struct{}

func (s *stubErrorNormalizer) Normalize(_ int, _ http.Header, _ []byte) *ProviderError {
	return &ProviderError{Code: CodeUpstreamError}
}
