package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestExtractIngressModel_OpenAI_FromBody(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}
	body := []byte(`{"model":"gpt-4o","stream":true,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", model)
	}
	if !stream {
		t.Errorf("stream = false, want true")
	}
}

func TestExtractIngressModel_Cohere_Rerank_FromBody(t *testing.T) {
	// /v1/rerank canonical = Cohere shape, model in the body. Without the
	// FormatCohere case the ingress fell to the default arm and 400'd with
	// "unsupported ingress format cohere".
	in := Ingress{WireShape: typology.WireShapeCohereRerank, BodyFormat: provcore.FormatCohere}
	body := []byte(`{"model":"rerank-v3.5","query":"q","documents":["a","b"]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", bytes.NewReader(body))

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "rerank-v3.5" {
		t.Errorf("model = %q, want rerank-v3.5", model)
	}
	if stream {
		t.Errorf("stream = true, want false (rerank is non-streaming)")
	}
}

func TestExtractIngressModel_Anthropic_FromBody(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeAnthropicMessages, BodyFormat: provcore.FormatAnthropic}
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "claude-3-5-sonnet" {
		t.Errorf("model = %q, want claude-3-5-sonnet", model)
	}
	if stream {
		t.Errorf("stream = true, want false")
	}
}

func TestExtractIngressModel_Gemini_FromPath_NonStreaming(t *testing.T) {
	in := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatGemini,
	}
	body := []byte(`{"contents":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", bytes.NewReader(body))
	req.SetPathValue("model", "gemini-1.5-pro:generateContent")

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "gemini-1.5-pro" {
		t.Errorf("model = %q, want gemini-1.5-pro", model)
	}
	if stream {
		t.Errorf("stream = true, want false (non-streaming path)")
	}
}

func TestExtractIngressModel_Gemini_FromPath_Streaming(t *testing.T) {
	in := Ingress{
		WireShape:      typology.WireShapeOpenAIChat,
		BodyFormat:     provcore.FormatGemini,
		Stream:         true,
		StreamFromPath: true,
	}
	body := []byte(`{"contents":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:streamGenerateContent", bytes.NewReader(body))
	req.SetPathValue("model", "gemini-1.5-pro:streamGenerateContent")

	_, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !stream {
		t.Errorf("stream = false, want true (streaming path)")
	}
}

func TestExtractIngressModel_Gemini_MissingPathValue(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeGeminiGenerateContent, BodyFormat: provcore.FormatGemini}
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/:generateContent", nil)

	if _, _, err := ExtractIngressModel(in, req, nil); err == nil {
		t.Fatalf("expected error for missing {model}, got nil")
	}
}

func TestExtractIngressModel_Gemini_InvalidPathSuffix(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeGeminiGenerateContent, BodyFormat: provcore.FormatGemini}
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:unknown", nil)
	req.SetPathValue("model", "gemini-1.5-pro:unknown")

	if _, _, err := ExtractIngressModel(in, req, nil); err == nil {
		t.Fatalf("expected error for invalid path suffix, got nil")
	}
}

func TestExtractIngressModel_Azure_FromPath(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatAzureOpenAI}
	body := []byte(`{"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-4-turbo/chat/completions", bytes.NewReader(body))
	req.SetPathValue("deployment", "gpt-4-turbo")

	model, _, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "gpt-4-turbo" {
		t.Errorf("model = %q, want gpt-4-turbo", model)
	}
}

func TestExtractIngressModel_BedrockVertex_Rejected(t *testing.T) {
	for _, f := range []provcore.Format{provcore.FormatBedrock, provcore.FormatVertex} {
		in := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: f}
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if _, _, err := ExtractIngressModel(in, req, nil); err == nil {
			t.Errorf("format %q: expected error, got nil", f)
		}
	}
}

// TestExtractIngressModel_Responses_FromBody pins the /v1/responses ingress
// model extraction. /v1/responses uses the same top-level `model` and
// `stream` fields as chat-completions (just with `input` instead of
// `messages` for the prompt — which we don't extract here). The pre-fix
// bug returned `unsupported ingress format "openai-responses"` in prod.
func TestExtractIngressModel_Responses_FromBody(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeOpenAIResponses, BodyFormat: provcore.FormatOpenAIResponses}
	body := []byte(`{"model":"gpt-5.2","input":"hi","stream":true,"max_output_tokens":10}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("Responses ingress unexpectedly errored: %v", err)
	}
	if model != "gpt-5.2" {
		t.Errorf("model = %q, want gpt-5.2", model)
	}
	if !stream {
		t.Errorf("stream = false, want true (body said stream:true)")
	}

	// Non-stream variant.
	body2 := []byte(`{"model":"gpt-4o","input":"hello","max_output_tokens":5}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body2))
	model2, stream2, err := ExtractIngressModel(in, req2, body2)
	if err != nil {
		t.Fatalf("Responses non-stream errored: %v", err)
	}
	if model2 != "gpt-4o" || stream2 {
		t.Errorf("non-stream Responses: model=%q stream=%v (want gpt-4o, false)", model2, stream2)
	}
}

// TestExtractIngressModel_TrimsTheClientString — the model string is compared
// byte-for-byte in three places that each fail differently when it carries
// padding: the catalogue lookup answers "does not exist" about a model that
// does, routing's requestedModelLiterals glob misses the rule written for the
// request, and the audit row's model_name splits one model across two analytics
// keys. Trimmed once at the single exit so a new ingress format cannot forget.
func TestExtractIngressModel_TrimsTheClientString(t *testing.T) {
	for _, tc := range []struct {
		name, sent, want string
	}{
		{"leading and trailing spaces", `{"model":"  gpt-4o  "}`, "gpt-4o"},
		{"tab and newline", "{\"model\":\"\\tgpt-4o\\n\"}", "gpt-4o"},
		{"a keyword with padding", `{"model":" auto "}`, "auto"},
		{"already clean is untouched", `{"model":"gpt-4o"}`, "gpt-4o"},
		{"whitespace only becomes empty", `{"model":"   "}`, ""},
		{"inner spaces are preserved", `{"model":"my model"}`, "my model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Ingress{BodyFormat: provcore.FormatOpenAI}
			got, _, err := ExtractIngressModel(in, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(tc.sent))
			if err != nil {
				t.Fatalf("ExtractIngressModel: %v", err)
			}
			if got != tc.want {
				t.Errorf("model = %q, want %q", got, tc.want)
			}
		})
	}
}

// A whitespace-only model must be REFUSED, not routed. Nothing downstream
// trimmed it before, so it passed admission and reached the matcher, where only
// a rule someone had pinned to whitespace could serve it. Now it trims to empty
// and takes the caller's own errModelRequired path.
func TestExtractIngressModel_WhitespaceOnlyIsIndistinguishableFromAbsent(t *testing.T) {
	in := Ingress{BodyFormat: provcore.FormatOpenAI}
	blank, _, err := ExtractIngressModel(in, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(`{"model":"   "}`))
	if err != nil {
		t.Fatalf("ExtractIngressModel: %v", err)
	}
	absent, _, err := ExtractIngressModel(in, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(`{}`))
	if err != nil {
		t.Fatalf("ExtractIngressModel (absent): %v", err)
	}
	if blank != absent {
		t.Errorf("a whitespace-only model must reach admission as the absent case "+
			"(readBody answers errModelRequired on \"\"); got %q vs %q", blank, absent)
	}
}

// TestReadBody_DelegationKeywordsAreNotSpecialCasedByEndpoint — admission does
// not decide, per endpoint, which delegation keywords are allowed. It used to,
// for exactly one string on exactly one endpoint: `model: "auto"` on
// /v1/embeddings was refused outright.
//
// That line was born with smart routing, when the strategy was a chat-only LLM
// task-router. Since then prepareModelPool reads ListEnabledCandidates(kind),
// SmartStrategy short-circuits non-chat kinds to modalityAutoTargets, the
// modality guard drops cross-modality targets, and the embeddings capability
// pre-filter runs on whatever strategy produced the plan. Measured on prod
// 2026-09-03 with one rule pinned to [auto, janus:default]: the OTHER keyword
// already served embeddings (text-embedding-3-small, 1536 dims), rerank and
// image generation too. Only "auto" was held back, so the line refused a
// spelling rather than a risk.
//
// This is the shape of the assertion rather than a full request roundtrip:
// ExtractIngressModel is the sole model entrance, and if it or readBody ever
// reintroduces a per-endpoint keyword veto, the extracted value for an
// embeddings-shaped request stops matching the chat-shaped one.
func TestReadBody_DelegationKeywordsAreNotSpecialCasedByEndpoint(t *testing.T) {
	body := []byte(`{"model":"auto","input":"hello"}`)
	for _, shape := range []struct {
		name string
		wire typology.WireShape
	}{
		{"chat", typology.WireShapeOpenAIChat},
		{"embeddings", typology.WireShapeOpenAIEmbeddings},
	} {
		t.Run(shape.name, func(t *testing.T) {
			in := Ingress{WireShape: shape.wire, BodyFormat: provcore.FormatOpenAI}
			got, _, err := ExtractIngressModel(in, httptest.NewRequest(http.MethodPost, "/v1/x", nil), body)
			if err != nil {
				t.Fatalf("%s: ExtractIngressModel: %v", shape.name, err)
			}
			if got != "auto" {
				t.Errorf("%s: model = %q, want \"auto\" — the endpoint must not filter which "+
					"delegation keyword reaches routing", shape.name, got)
			}
		})
	}
}
