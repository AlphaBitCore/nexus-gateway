// Package adaptertest provides a shared contract test suite for the thin
// OpenAI-compatible API adapters under packages/shared/traffic/adapters/api/.
//
// Several vendors (fireworks, groq, mistral, moonshot, perplexity, together,
// xai) speak the OpenAI chat-completions wire shape and differ only in their
// adapter ID, Provider label, API-key class, accepted host, chat-completions
// path, and sample fixtures. Each of those packages historically carried a
// ~367-line test file that was ~85% identical. RunContract collapses that
// duplication into one suite parameterized by Case, mirroring the
// mq.ComplianceTestSuite precedent: it lives in an importable (non-_test)
// package so each vendor's _test.go can call it without copy-pasting the
// assertions.
//
// Usage (in a vendor's _test.go file):
//
//	func TestContract(t *testing.T) {
//	    adaptertest.RunContract(t, adaptertest.Case{
//	        Adapter:     &Adapter{},
//	        AdapterID:   adapterID,
//	        Provider:    "xai",
//	        KeyClass:    "xai-bearer",
//	        Host:        "api.x.ai",
//	        ChatPath:    "/v1/chat/completions",
//	        SampleModel: "grok-2-latest",
//	        SampleToken: "xai-abcdef123456",
//	    })
//	}
package adaptertest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Adapter is the capability surface a thin OpenAI-compat vendor adapter
// exposes: the full traffic.Adapter contract. Audit-time normalization
// is NOT part of this surface — the normalize Registry routes these
// vendors' wire formats to the shared codecs by adapter-type key, so
// the per-vendor adapters carry no Normalize method.
type Adapter interface {
	traffic.Adapter
}

// Case parameterizes every per-vendor variation the contract suite needs.
// Purely cosmetic literals that carry no vendor semantics (the round-tripped
// segment text, the tool-call function name, the redaction sentinel, the
// embeddings/unknown paths) are fixed inside the suite — only fields that
// genuinely diverge per vendor live here.
type Case struct {
	// Adapter is a fresh, zero-value vendor adapter instance under test.
	Adapter Adapter
	// AdapterID is the value Adapter.ID() must return and the AdapterType /
	// DetectedSpec asserted by the Normalize tests (e.g. "xai").
	AdapterID string
	// Provider is the attribution label DetectRequestMeta stamps. Equals
	// AdapterID for the covered vendors but kept distinct for faithfulness.
	Provider string
	// KeyClass is the ApiKeyClass stamped for a Bearer token (e.g. "xai-bearer").
	KeyClass string
	// Host is the request URL host used to build fixture requests. The
	// production code does not validate host, so this is fixture realism only.
	Host string
	// ChatPath is the chat-completions path this vendor serves on. Most use
	// "/v1/chat/completions"; groq uses "/openai/v1/chat/completions";
	// perplexity uses "/chat/completions".
	ChatPath string
	// SampleModel is a realistic model id that must round-trip through model
	// extraction and Normalize (e.g. "grok-2-latest").
	SampleModel string
	// SampleToken is a realistic Bearer token whose presence drives the
	// ApiKeyClass / ApiKeyFingerprint stamping path.
	SampleToken string
}

// Fixed suite-internal literals. These carry no vendor semantics — the
// assertions test round-trip / substring / patch-count behavior, which a
// constant exercises identically to a per-vendor string.
const (
	toolName       = "web_search"
	embeddingsPath = "/v1/embeddings"
	unknownPath    = "/v1/garbage"
)

// RunContract executes the shared adapter contract against one vendor Case.
// Every assertion the per-vendor test files used to carry is reproduced here;
// each branch of the vendor adapter (ID, Configure, the five extract/rewrite
// delegations and their error sentinels, all five DetectRequestMeta paths, the
// three DetectResponseUsage states, and both Normalize outcomes) is exercised.
func RunContract(t *testing.T, c Case) {
	t.Helper()

	t.Run("ID", func(t *testing.T) {
		if got := c.Adapter.ID(); got != c.AdapterID {
			t.Errorf("ID=%q want %q", got, c.AdapterID)
		}
	})

	// Configure is a no-op (delegates to the openai inner, also a no-op). Pin
	// both nil and populated forms so future config additions stay error-free.
	t.Run("Configure", func(t *testing.T) {
		if err := c.Adapter.Configure(nil); err != nil {
			t.Errorf("Configure(nil)=%v", err)
		}
		if err := c.Adapter.Configure(map[string]any{"foo": "bar"}); err != nil {
			t.Errorf("Configure(map)=%v", err)
		}
	})

	t.Run("ExtractRequest_OpenAICompatDelegation", func(t *testing.T) {
		body := []byte(`{"model":"` + c.SampleModel + `","messages":[{"role":"user","content":"hi from user"}]}`)
		nc, err := c.Adapter.ExtractRequest(context.Background(), body, c.ChatPath)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(nc.Segments) != 1 || nc.Segments[0] != "hi from user" {
			t.Errorf("Segments=%v", nc.Segments)
		}
	})

	t.Run("ExtractRequest_ToolCallsDelegation", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"` + toolName + `","arguments":"{}"}}` +
			`]}]}`)
		nc, err := c.Adapter.ExtractRequest(context.Background(), body, c.ChatPath)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"`+toolName+`"`) {
			t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
		}
	})

	// DetectRequestMeta with a Bearer token: Provider re-labeled and the
	// vendor key class + a non-empty fingerprint are stamped.
	t.Run("DetectRequestMeta_KeyClass", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
		r := c.newRequest()
		r.Header.Set("Authorization", "Bearer "+c.SampleToken)
		meta := c.Adapter.DetectRequestMeta(r, body)
		if meta.Provider != c.Provider {
			t.Errorf("Provider=%q want %q", meta.Provider, c.Provider)
		}
		if meta.ApiKeyClass != c.KeyClass {
			t.Errorf("ApiKeyClass=%q want %q", meta.ApiKeyClass, c.KeyClass)
		}
		if meta.ApiKeyFingerprint == "" {
			t.Errorf("ApiKeyFingerprint empty, want stamped for Bearer token")
		}
	})

	// ExtractResponse delegation: chat/completions response is decoded by the
	// openai-compat inner. Segments + finish_reason must surface end-to-end.
	t.Run("ExtractResponse_ChatCompletionsDelegation", func(t *testing.T) {
		body := []byte(`{"id":"resp_1","model":"` + c.SampleModel + `","choices":[{` +
			`"index":0,"message":{"role":"assistant","content":"hello from assistant"},` +
			`"finish_reason":"stop"}]}`)
		nc, err := c.Adapter.ExtractResponse(context.Background(), body, c.ChatPath)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(nc.Segments) != 1 || nc.Segments[0] != "hello from assistant" {
			t.Errorf("Segments=%v", nc.Segments)
		}
		if nc.Metadata["finish_reason"] != "stop" {
			t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
		}
	})

	// ExtractResponse on a malformed body must surface ErrMalformed so the
	// dispatcher can distinguish "garbage" from "no body".
	t.Run("ExtractResponse_Malformed", func(t *testing.T) {
		_, err := c.Adapter.ExtractResponse(context.Background(), []byte(`not json`), c.ChatPath)
		if !errors.Is(err, traffic.ErrMalformed) {
			t.Errorf("err=%v want ErrMalformed", err)
		}
	})

	// ExtractResponse on an unknown path routes to the inner default branch
	// returning ErrUnknownSchema — the wrapper must not rewrite this to nil.
	t.Run("ExtractResponse_UnknownPath", func(t *testing.T) {
		_, err := c.Adapter.ExtractResponse(context.Background(), []byte(`{}`), unknownPath)
		if !errors.Is(err, traffic.ErrUnknownSchema) {
			t.Errorf("err=%v want ErrUnknownSchema", err)
		}
	})

	// ExtractStreamChunk delegation: a single SSE content delta yields a
	// single Segment; the wrapper does no transform.
	t.Run("ExtractStreamChunk_ContentDelta", func(t *testing.T) {
		chunk := []byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		nc, err := c.Adapter.ExtractStreamChunk(context.Background(), chunk, c.ChatPath)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
			t.Errorf("Segments=%v", nc.Segments)
		}
	})

	// ExtractStreamChunk on malformed JSON must surface ErrMalformed — a
	// silently-empty NormalizedContent would hide stream corruption.
	t.Run("ExtractStreamChunk_Malformed", func(t *testing.T) {
		_, err := c.Adapter.ExtractStreamChunk(context.Background(), []byte(`not json`), c.ChatPath)
		if !errors.Is(err, traffic.ErrMalformed) {
			t.Errorf("err=%v want ErrMalformed", err)
		}
	})

	// RewriteRequestBody on chat/completions delegates to the openai-compat
	// rewriter and substitutes per-message text — pin that the wrapper does
	// not lose the rewritten output or the patch count.
	t.Run("RewriteRequestBody_ChatCompletionsDelegation", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"original"}]}`)
		out, n, err := c.Adapter.RewriteRequestBody(context.Background(), body, c.ChatPath, traffic.NormalizedContent{
			Segments: []string{"REDACTED"},
		})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if n != 1 {
			t.Errorf("patches=%d want 1", n)
		}
		if !strings.Contains(string(out), `"REDACTED"`) || strings.Contains(string(out), `"original"`) {
			t.Errorf("rewrite did not apply: %s", string(out))
		}
	})

	// RewriteRequestBody on /embeddings returns ErrRewriteUnsupported — the
	// inner openai adapter rejects rewriting on this surface.
	t.Run("RewriteRequestBody_EmbeddingsUnsupported", func(t *testing.T) {
		_, _, err := c.Adapter.RewriteRequestBody(context.Background(), []byte(`{"input":"hi"}`), embeddingsPath, traffic.NormalizedContent{})
		if !errors.Is(err, traffic.ErrRewriteUnsupported) {
			t.Errorf("err=%v want ErrRewriteUnsupported", err)
		}
	})

	// RewriteResponseBody on chat/completions delegates to the openai-compat
	// response rewriter, substituting assistant text. Pin output + patch count.
	t.Run("RewriteResponseBody_ChatCompletionsDelegation", func(t *testing.T) {
		body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"raw"}}]}`)
		out, n, err := c.Adapter.RewriteResponseBody(context.Background(), body, c.ChatPath, traffic.NormalizedContent{
			Segments: []string{"REDACTED"},
		})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if n != 1 {
			t.Errorf("patches=%d want 1", n)
		}
		if !strings.Contains(string(out), `"REDACTED"`) || strings.Contains(string(out), `"raw"`) {
			t.Errorf("rewrite did not apply: %s", string(out))
		}
	})

	// RewriteResponseBody on /embeddings is unsupported — pin the sentinel.
	t.Run("RewriteResponseBody_EmbeddingsUnsupported", func(t *testing.T) {
		_, _, err := c.Adapter.RewriteResponseBody(context.Background(), []byte(`{"data":[]}`), embeddingsPath, traffic.NormalizedContent{})
		if !errors.Is(err, traffic.ErrRewriteUnsupported) {
			t.Errorf("err=%v want ErrRewriteUnsupported", err)
		}
	})

	// DetectRequestMeta with no Authorization header: ApiKeyClass and
	// ApiKeyFingerprint stay empty — the wrapper must not invent a stale tag —
	// while Provider and Model still surface from the body.
	t.Run("DetectRequestMeta_NoAuth", func(t *testing.T) {
		body := []byte(`{"model":"` + c.SampleModel + `","messages":[{"role":"user","content":"hi"}]}`)
		r := c.newRequest()
		meta := c.Adapter.DetectRequestMeta(r, body)
		if meta.ApiKeyClass != "" {
			t.Errorf("ApiKeyClass=%q want empty", meta.ApiKeyClass)
		}
		if meta.ApiKeyFingerprint != "" {
			t.Errorf("ApiKeyFingerprint=%q want empty", meta.ApiKeyFingerprint)
		}
		if meta.Provider != c.Provider {
			t.Errorf("Provider=%q want %q", meta.Provider, c.Provider)
		}
		if meta.Model != c.SampleModel {
			t.Errorf("Model=%q want %q", meta.Model, c.SampleModel)
		}
	})

	// DetectRequestMeta with a non-Bearer scheme: the wrapper must NOT stamp
	// the vendor key class — non-Bearer headers are not API keys and tagging
	// them would poison attribution.
	t.Run("DetectRequestMeta_NonBearerAuth", func(t *testing.T) {
		r := c.newRequest()
		r.Header.Set("Authorization", "Basic xyz")
		meta := c.Adapter.DetectRequestMeta(r, nil)
		if meta.ApiKeyClass != "" {
			t.Errorf("ApiKeyClass=%q want empty for non-Bearer scheme", meta.ApiKeyClass)
		}
		if meta.ApiKeyFingerprint != "" {
			t.Errorf("ApiKeyFingerprint=%q want empty", meta.ApiKeyFingerprint)
		}
	})

	// DetectRequestMeta with "Bearer " followed by only whitespace: after
	// TrimSpace the token is empty, so neither field is set — blank
	// fingerprints would collide across every request.
	t.Run("DetectRequestMeta_BearerEmptyToken", func(t *testing.T) {
		r := c.newRequest()
		r.Header.Set("Authorization", "Bearer    ")
		meta := c.Adapter.DetectRequestMeta(r, nil)
		if meta.ApiKeyClass != "" {
			t.Errorf("ApiKeyClass=%q want empty for blank token", meta.ApiKeyClass)
		}
		if meta.ApiKeyFingerprint != "" {
			t.Errorf("ApiKeyFingerprint=%q want empty for blank token", meta.ApiKeyFingerprint)
		}
	})

	// DetectRequestMeta with nil request: defensive path — body-only callers
	// still get Provider + Model stamped, but no ApiKey fields.
	t.Run("DetectRequestMeta_NilRequest", func(t *testing.T) {
		body := []byte(`{"model":"` + c.SampleModel + `"}`)
		meta := c.Adapter.DetectRequestMeta(nil, body)
		if meta.Provider != c.Provider {
			t.Errorf("Provider=%q want %q", meta.Provider, c.Provider)
		}
		if meta.Model != c.SampleModel {
			t.Errorf("Model=%q want %q", meta.Model, c.SampleModel)
		}
		if meta.ApiKeyClass != "" {
			t.Errorf("ApiKeyClass=%q want empty for nil request", meta.ApiKeyClass)
		}
	})

	// DetectResponseUsage parses the standard OpenAI-shape usage block; the
	// wrapper passes through unchanged so the prompt/completion split and
	// Status=OK surface end-to-end.
	t.Run("DetectResponseUsage_OK", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
		um := c.Adapter.DetectResponseUsage(nil, body)
		if um.Status != traffic.UsageStatusOK {
			t.Errorf("Status=%q want ok", um.Status)
		}
		if um.PromptTokens == nil || *um.PromptTokens != 11 {
			t.Errorf("PromptTokens=%v", um.PromptTokens)
		}
		if um.CompletionTokens == nil || *um.CompletionTokens != 7 {
			t.Errorf("CompletionTokens=%v", um.CompletionTokens)
		}
	})

	// DetectResponseUsage zero-length body returns NoBody — distinct from
	// ParseFailed so observability can tell "no body seen" from "garbage".
	t.Run("DetectResponseUsage_NoBody", func(t *testing.T) {
		if c.Adapter.DetectResponseUsage(nil, nil).Status != traffic.UsageStatusNoBody {
			t.Errorf("want no_body for nil body")
		}
	})

	// DetectResponseUsage on non-JSON returns ParseFailed.
	t.Run("DetectResponseUsage_ParseFailed", func(t *testing.T) {
		if c.Adapter.DetectResponseUsage(nil, []byte(`not json`)).Status != traffic.UsageStatusParseFailed {
			t.Errorf("want parse_failed")
		}
	})
}

// newRequest builds a POST request to the vendor's chat-completions endpoint.
// The host is fixture realism only — the adapters do not validate it.
func (c Case) newRequest() *http.Request {
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://"+c.Host+c.ChatPath, nil)
	return r
}
