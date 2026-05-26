// Package voyage implements the Voyage AI traffic adapter.
//
// Voyage AI is an embedding-only provider (https://api.voyageai.com/v1/embeddings).
// The request body uses `input` (string | []string) and the response carries
// `data[].embedding` in standard OpenAI-compatible shape. This adapter
// treats Voyage traffic as a close sibling of the OpenAI embeddings shape.
//
// Wire format reference (Voyage AI API):
//
//	Request:  {model, input: string|[]string, input_type?, truncation?, output_dimension?, output_dtype?}
//	Response: {object:"list", data:[{object:"embedding", embedding:[...], index:0}],
//	           model, usage:{total_tokens:N}}
package voyage

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "voyage"

var requestKnownKeys = []string{
	"model", "input", "input_type", "truncation", "output_dimension", "output_dtype",
}

var responseKnownKeys = []string{
	"object", "data", "model", "usage",
}

// Adapter implements [traffic.Adapter] for Voyage AI's /v1/embeddings surface.
type Adapter struct{}

func (a *Adapter) ID() string                       { return adapterID }
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a Voyage AI embeddings request body.
//
// Input may be a bare string or an array of strings; both are collected
// into Segments so the compliance pipeline can inspect them.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	inputVal := gjson.GetBytes(body, "input")
	if !inputVal.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments []string
	switch {
	case inputVal.Type == gjson.String:
		segments = []string{inputVal.Str}
	case inputVal.IsArray():
		inputVal.ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				segments = append(segments, v.Str)
			}
			return true
		})
	default:
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta["model"] = model.Str
	}
	if it := gjson.GetBytes(body, "input_type"); it.Type == gjson.String && it.Str != "" {
		meta["input_type"] = it.Str
	}

	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// ExtractResponse parses a Voyage AI embeddings response body.
//
// Voyage returns the canonical OpenAI embeddings shape (data[].embedding).
// Embedding vectors are intentionally not stored in Segments per SDD §T2.3;
// we only capture usage metadata.
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if !gjson.GetBytes(body, "data").IsArray() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String && model.Str != "" {
		meta["model"] = model.Str
	}

	return traffic.NormalizedContent{
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, responseKnownKeys),
	}, nil
}

// ExtractStreamChunk is a no-op for Voyage AI — the embeddings endpoint
// does not emit streaming responses.
func (a *Adapter) ExtractStreamChunk(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
	return traffic.NormalizedContent{}, nil
}

// DetectRequestMeta extracts provider, model, and API-key fingerprint.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "voyage"}
	if r != nil {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				meta.ApiKeyClass = "voyage-bearer"
				meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
			}
		}
	}
	if gjson.ValidBytes(body) {
		if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
			meta.Model = model.Str
		}
	}
	return meta
}

// DetectResponseUsage extracts Voyage AI's usage block.
//
//	{"usage": {"total_tokens": N}}
//
// Voyage reports only total_tokens (no separate prompt/completion split
// for embeddings). We store it in PromptTokens per the canonical convention
// that for embedding requests, prompt_tokens == total_tokens.
func (a *Adapter) DetectResponseUsage(_ *http.Response, body []byte) traffic.UsageMeta {
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	if !gjson.ValidBytes(body) {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}
	usage := traffic.UsageMeta{Status: traffic.UsageStatusOK}
	if tt := gjson.GetBytes(body, "usage.total_tokens"); tt.Exists() && tt.Type == gjson.Number {
		v := int(tt.Int())
		usage.PromptTokens = &v
	}
	if usage.PromptTokens == nil {
		usage.Status = traffic.UsageStatusParseFailed
	}
	return usage
}

func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
