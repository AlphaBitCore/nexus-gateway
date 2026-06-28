// Package huggingface implements the huggingface traffic adapter for
// the Hugging Face Inference API (api-inference.huggingface.co) and
// Inference Endpoints (*.endpoints.huggingface.cloud).
//
// Hugging Face supports multiple wire formats depending on the
// endpoint type:
//   - Text Generation Inference (TGI) endpoints expose
//     /v1/chat/completions with OpenAI-compatible wire format.
//   - Legacy serverless inference endpoints use task-specific shapes
//     such as {"inputs": "<text>"} for text-generation /
//     summarization / etc., and {"inputs": [<rows>]} for table-qa.
//
// The adapter detects which shape is in use and routes accordingly.
// Replaces the generic-jsonpath placeholder used in the original seed.
package huggingface

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

const adapterID = "huggingface"

// Adapter dispatches between the OpenAI-compat path (TGI endpoints)
// and the legacy serverless path based on body shape.
type Adapter struct {
	openaiInner openai.Adapter
}

func (a *Adapter) ID() string                         { return adapterID }
func (a *Adapter) Configure(cfg map[string]any) error { return a.openaiInner.Configure(cfg) }

// hasMessages reports whether the body looks like an OpenAI-compat
// chat-completions request (top-level `messages` array).
func hasMessages(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	return gjson.GetBytes(body, "messages").IsArray()
}

func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if hasMessages(body) {
		return a.openaiInner.ExtractRequest(ctx, body, path)
	}
	// Legacy serverless: top-level `inputs` is required.
	inputs := gjson.GetBytes(body, "inputs")
	if !inputs.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	var segments []string
	switch {
	case inputs.Type == gjson.String:
		segments = append(segments, inputs.Str)
	case inputs.IsArray():
		inputs.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				segments = append(segments, item.Str)
			}
			return true
		})
	}
	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta["model"] = model.Str
	}
	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
	}, nil
}

func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	// TGI / OpenAI-compat response.
	if gjson.GetBytes(body, "choices").IsArray() {
		return a.openaiInner.ExtractResponse(ctx, body, path)
	}
	// Legacy serverless responses are most often arrays of generation results:
	// [{"generated_text":"..."}], [{"summary_text":"..."}], etc.
	root := gjson.ParseBytes(body)
	if root.IsArray() {
		var segments []string
		root.ForEach(func(_, item gjson.Result) bool {
			for _, key := range []string{"generated_text", "summary_text", "translation_text", "answer"} {
				if t := item.Get(key); t.Type == gjson.String && t.Str != "" {
					segments = append(segments, t.Str)
				}
			}
			return true
		})
		if len(segments) > 0 {
			return traffic.NormalizedContent{Segments: segments}, nil
		}
	}
	// Object-shaped response (e.g. fill-mask): {"sequence":"..."} etc.
	if root.IsObject() {
		for _, key := range []string{"generated_text", "summary_text", "translation_text", "answer"} {
			if t := gjson.GetBytes(body, key); t.Type == gjson.String && t.Str != "" {
				return traffic.NormalizedContent{Segments: []string{t.Str}}, nil
			}
		}
		// Error envelope.
		if errMsg := gjson.GetBytes(body, "error"); errMsg.Type == gjson.String && errMsg.Str != "" {
			return traffic.NormalizedContent{Segments: []string{errMsg.Str}, Metadata: map[string]string{"error": "true"}}, nil
		}
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	// TGI-OpenAI-compat streams use the standard delta shape; the
	// openai-compat adapter handles them. Legacy serverless streams
	// for text-generation use {"token":{"text":"..."}} per chunk plus
	// a final {"generated_text":"..."} marker.
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if gjson.GetBytes(chunk, "choices").IsArray() {
		return a.openaiInner.ExtractStreamChunk(ctx, chunk, path)
	}
	var segments []string
	if t := gjson.GetBytes(chunk, "token.text"); t.Type == gjson.String && t.Str != "" {
		segments = append(segments, t.Str)
	}
	// The "generated_text" field is a final aggregate marker; the per-token
	// chunks already covered the content, so it is intentionally ignored
	// here to avoid double-counting.
	return traffic.NormalizedContent{Segments: segments}, nil
}

func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "huggingface"}
	if r != nil {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				if strings.HasPrefix(tok, "hf_") {
					meta.ApiKeyClass = "huggingface-token"
				} else {
					meta.ApiKeyClass = "huggingface-bearer"
				}
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

func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	// TGI-OpenAI-compat responses include `usage`. Legacy serverless
	// responses do not surface token counts.
	if r != nil && gjson.ValidBytes(body) && gjson.GetBytes(body, "choices").IsArray() {
		return a.openaiInner.DetectResponseUsage(r, body)
	}
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
}

func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
