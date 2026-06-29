// Package bedrock implements the traffic adapter for Amazon Bedrock.
// Bedrock multiplexes multiple provider formats (Anthropic, Cohere, AI21,
// Meta, Titan) under one API surface. The URL path encodes the upstream
// model id — e.g. `/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke`
// — and the request body uses the upstream provider's native schema.
//
// Coverage:
//   - `anthropic.*` model ids → delegated to the anthropic adapter
//     (current spec: text + tool_use + thinking). The bulk of
//     enterprise usage.
//   - `meta.llama*` model ids → native Llama-on-Bedrock parsing
//     (single-prompt request / single-generation response, stream
//     emits one JSON line per token).
//   - Other formats (Cohere, AI21, Titan) return ErrUnknownSchema;
//     detect still attributes provider/model/auth even when content
//     extraction is skipped.
//
// Authentication is AWS SigV4. The `Authorization` header has the shape
// `AWS4-HMAC-SHA256 Credential=AKIA<…>/<date>/<region>/<service>/aws4_request, …`
// where <service> is `bedrock-runtime` for InvokeModel /
// InvokeModelWithResponseStream / Converse and `bedrock` only for the
// control-plane endpoints. We extract the AKID (the segment before the
// first `/`) as the fingerprint source — it is already a public
// identifier safe to store, but we hash it for uniformity.
package bedrock

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
)

// bedrockModelPathPattern extracts the modelId segment from paths such as
// `/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke`.
var bedrockModelPathPattern = regexp.MustCompile(`/model/([^/]+)/(?:invoke|invoke-with-response-stream|converse|converse-stream)`)

// sigV4CredentialPattern matches the AKID at the start of the
// Credential clause, e.g.
// `Credential=AKIA…/20260421/us-east-1/bedrock-runtime/aws4_request`.
// The regex stops at the first `/` so it works for both runtime
// (`bedrock-runtime`) and control-plane (`bedrock`) service scopes.
var sigV4CredentialPattern = regexp.MustCompile(`Credential=([A-Z0-9]{16,32})/`)

// Adapter implements Bedrock content extraction by delegating to the
// anthropic adapter for anthropic.* models.
type Adapter struct {
	anthropic anthropic.Adapter
}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "bedrock" }

// Configure is a no-op.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest dispatches by model family: anthropic.* delegates to
// the anthropic adapter; meta.llama* runs the native Llama parser;
// other upstream formats return ErrUnknownSchema.
func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if isAnthropicModel(path) {
		return a.anthropic.ExtractRequest(ctx, body, path)
	}
	if isLlamaModel(path) {
		return extractLlamaRequest(body, path)
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractResponse dispatches by model family.
func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if isAnthropicModel(path) {
		return a.anthropic.ExtractResponse(ctx, body, path)
	}
	if isLlamaModel(path) {
		return extractLlamaResponse(body)
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractStreamChunk dispatches by model family. Bedrock streams Llama
// as JSON-lines (one event per line); each chunk reaches us as a
// single parsed JSON event.
func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	if isAnthropicModel(path) {
		return a.anthropic.ExtractStreamChunk(ctx, chunk, path)
	}
	if isLlamaModel(path) {
		return extractLlamaStreamChunk(chunk)
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// extractLlamaRequest parses a Meta-Llama-on-Bedrock request body. The
// shape is minimal: a single `prompt` string plus generation params.
// The prompt is the entire user-facing input (Llama on Bedrock has no
// structured messages array; conversation history is concatenated by
// the caller using Llama's chat template).
func extractLlamaRequest(body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	prompt := gjson.GetBytes(body, "prompt")
	if !prompt.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	var segments []string
	if prompt.Type == gjson.String && prompt.Str != "" {
		segments = append(segments, prompt.Str)
	}
	return traffic.NormalizedContent{
		Segments: segments,
	}, nil
}

// extractLlamaResponse parses a Llama-on-Bedrock non-streaming response.
// `generation` carries the assistant's output; `stop_reason` lands on
// Metadata. Token counts on this path are extracted by
// DetectResponseUsage; we just surface stop_reason here.
func extractLlamaResponse(body []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	gen := gjson.GetBytes(body, "generation")
	if !gen.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	var segments []string
	if gen.Type == gjson.String && gen.Str != "" {
		segments = append(segments, gen.Str)
	}
	meta := map[string]string{}
	if sr := gjson.GetBytes(body, "stop_reason"); sr.Type == gjson.String && sr.Str != "" {
		meta["stop_reason"] = sr.Str
	}
	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
	}, nil
}

// extractLlamaStreamChunk parses one streaming chunk from a Llama-on-
// Bedrock InvokeModelWithResponseStream call. Each chunk carries the
// shape `{"generation":"<delta>","prompt_token_count":N,"generation_token_count":N,"stop_reason":null|"stop"}`.
func extractLlamaStreamChunk(chunk []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	var segments []string
	if g := gjson.GetBytes(chunk, "generation"); g.Type == gjson.String && g.Str != "" {
		segments = append(segments, g.Str)
	}
	var meta map[string]string
	if sr := gjson.GetBytes(chunk, "stop_reason"); sr.Type == gjson.String && sr.Str != "" {
		meta = map[string]string{"stop_reason": sr.Str}
	}
	return traffic.NormalizedContent{Segments: segments, Metadata: meta}, nil
}

// RewriteRequestBody delegates to the anthropic adapter for anthropic.*
// Bedrock models. Other publisher formats (Titan, Cohere, Meta) are
// already unsupported by ExtractRequest, so Rewrite cannot run for them
// — return ErrRewriteUnsupported and let the caller fall back to the
// original body.
func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	if isAnthropicModel(path) {
		return a.anthropic.RewriteRequestBody(ctx, body, path, content)
	}
	return nil, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody delegates to the anthropic adapter for anthropic.* paths.
func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	if isAnthropicModel(path) {
		return a.anthropic.RewriteResponseBody(ctx, body, path, content)
	}
	return nil, 0, traffic.ErrRewriteUnsupported
}

// DetectRequestMeta extracts provider="bedrock", model from URL path, and
// ApiKey* from the SigV4 Credential= segment (AKID).
func (a *Adapter) DetectRequestMeta(r *http.Request, _ []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "bedrock"}
	if r != nil {
		meta.Path = r.URL.Path
		meta.Model = modelFromBedrockPath(r.URL.Path)
		if akid := extractSigV4AKID(r.Header.Get("Authorization")); akid != "" {
			meta.ApiKeyClass = "aws-sigv4"
			meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(akid)
		}
	}
	return meta
}

// DetectResponseUsage delegates to anthropic for anthropic.* model paths.
// Llama-on-Bedrock surfaces token counts on the response body itself.
// Other models fall back to parse_failed.
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	if r != nil && isAnthropicModel(r.Request.URL.Path) {
		return a.anthropic.DetectResponseUsage(r, body)
	}
	if r != nil && isLlamaModel(r.Request.URL.Path) {
		return detectLlamaUsage(body)
	}
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
}

// detectLlamaUsage parses prompt_token_count + generation_token_count
// from a Llama-on-Bedrock non-streaming response.
func detectLlamaUsage(body []byte) traffic.UsageMeta {
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	if !gjson.ValidBytes(body) {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}
	usage := traffic.UsageMeta{Status: traffic.UsageStatusOK}
	if pt := gjson.GetBytes(body, "prompt_token_count"); pt.Exists() && pt.Type == gjson.Number {
		v := int(pt.Int())
		usage.PromptTokens = &v
	}
	if ct := gjson.GetBytes(body, "generation_token_count"); ct.Exists() && ct.Type == gjson.Number {
		v := int(ct.Int())
		usage.CompletionTokens = &v
	}
	if usage.PromptTokens == nil && usage.CompletionTokens == nil {
		usage.Status = traffic.UsageStatusParseFailed
	}
	return usage
}

// modelFromBedrockPath extracts the modelId segment.
func modelFromBedrockPath(path string) string {
	m := bedrockModelPathPattern.FindStringSubmatch(path)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// isAnthropicModel reports whether the modelId in the path belongs to
// Anthropic's Bedrock namespace.
func isAnthropicModel(path string) bool {
	model := modelFromBedrockPath(path)
	return strings.HasPrefix(model, "anthropic.")
}

// isLlamaModel reports whether the modelId in the path belongs to
// Meta's Llama-on-Bedrock namespace (e.g.
// `meta.llama3-70b-instruct-v1:0`, `meta.llama3-2-90b-instruct-v1:0`).
func isLlamaModel(path string) bool {
	model := modelFromBedrockPath(path)
	return strings.HasPrefix(model, "meta.llama")
}

// extractSigV4AKID pulls the access key id from a SigV4 Authorization header.
// Returns "" when the header is absent or not a SigV4 v1 signature.
func extractSigV4AKID(header string) string {
	if !strings.HasPrefix(header, "AWS4-HMAC-SHA256") {
		return ""
	}
	m := sigV4CredentialPattern.FindStringSubmatch(header)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
