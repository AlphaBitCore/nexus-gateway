// Package replicate implements the replicate traffic adapter for
// Replicate's prediction API at api.replicate.com.
//
// Replicate's API is uniquely poll-based: clients POST to
// /v1/predictions to start a prediction, then GET back the same
// resource to poll for status. The wire format is documented:
//   - Request: {"version": "<model-version-hash>", "input": {<schema>}}
//     where input contains the model's task-specific parameters
//     (most chat models use "prompt", "messages", "system_prompt", etc.)
//   - Response: {"id":"...","status":"starting|processing|succeeded|failed|canceled",
//     "input": {...}, "output": <task-specific>, ...}
//
// Replaces the generic-jsonpath placeholder used in the original seed.
package replicate

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "replicate"

var requestKnownKeys = []string{
	"version", "input", "model", "stream", "webhook", "webhook_events_filter",
}

var responseKnownKeys = []string{
	"id", "version", "input", "output", "status", "logs",
	"created_at", "started_at", "completed_at", "error",
	"urls", "data_removed", "metrics",
}

type Adapter struct{}

func (a *Adapter) ID() string                       { return adapterID }
func (a *Adapter) Configure(_ map[string]any) error { return nil }

func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	var segments, toolCalls []string
	// Common chat-model input fields:
	//   prompt, system_prompt, system, message
	for _, key := range []string{"prompt", "system_prompt", "system", "message", "query", "text"} {
		if v := input.Get(key); v.Type == gjson.String && v.Str != "" {
			segments = append(segments, v.Str)
		}
	}
	// messages array (some chat models on Replicate)
	if msgs := input.Get("messages"); msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			if c := msg.Get("content"); c.Type == gjson.String {
				segments = append(segments, c.Str)
			}
			if tc := msg.Get("tool_calls"); tc.IsArray() {
				tc.ForEach(func(_, call gjson.Result) bool {
					toolCalls = append(toolCalls, call.Raw)
					return true
				})
			}
			return true
		})
	}
	meta := map[string]string{}
	if v := gjson.GetBytes(body, "version"); v.Type == gjson.String && v.Str != "" {
		meta["version"] = v.Str
	}
	if m := gjson.GetBytes(body, "model"); m.Type == gjson.String && m.Str != "" {
		meta["model"] = m.Str
	}
	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
		Extra:            traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if !gjson.GetBytes(body, "id").Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
	var segments []string
	out := gjson.GetBytes(body, "output")
	switch {
	case out.Type == gjson.String:
		segments = append(segments, out.Str)
	case out.IsArray():
		out.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				segments = append(segments, item.Str)
			}
			return true
		})
	case out.IsObject():
		// Some chat models return {"text":"..."} or {"answer":"..."}.
		for _, key := range []string{"text", "answer", "completion", "message"} {
			if v := out.Get(key); v.Type == gjson.String && v.Str != "" {
				segments = append(segments, v.Str)
			}
		}
	}
	meta := map[string]string{}
	if status := gjson.GetBytes(body, "status"); status.Type == gjson.String && status.Str != "" {
		meta["status"] = status.Str
	}
	if id := gjson.GetBytes(body, "id"); id.Type == gjson.String && id.Str != "" {
		meta["id"] = id.Str
	}
	if errStr := gjson.GetBytes(body, "error"); errStr.Type == gjson.String && errStr.Str != "" {
		segments = append(segments, errStr.Str)
		meta["error"] = "true"
	}
	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, responseKnownKeys),
	}, nil
}

func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	// Replicate streaming uses SSE with output frames; the per-frame
	// shape depends on the model. Common fallbacks:
	//   {"output":"..."} or {"text":"..."}
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}
	var segments []string
	if out := gjson.GetBytes(chunk, "output"); out.Type == gjson.String && out.Str != "" {
		segments = append(segments, out.Str)
	}
	if t := gjson.GetBytes(chunk, "text"); t.Type == gjson.String && t.Str != "" {
		segments = append(segments, t.Str)
	}
	return traffic.NormalizedContent{Segments: segments}, nil
}

func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "replicate"}
	if r != nil {
		auth := r.Header.Get("Authorization")
		// Replicate uses both `Token <key>` and `Bearer <key>` formats.
		if strings.HasPrefix(auth, "Token ") {
			tok := strings.TrimSpace(auth[len("Token "):])
			if tok != "" {
				if strings.HasPrefix(tok, "r8_") {
					meta.ApiKeyClass = "replicate-token-r8"
				} else {
					meta.ApiKeyClass = "replicate-token"
				}
				meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
			}
		} else if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				meta.ApiKeyClass = "replicate-bearer"
				meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
			}
		}
	}
	if gjson.ValidBytes(body) {
		if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
			meta.Model = model.Str
		} else if v := gjson.GetBytes(body, "version"); v.Type == gjson.String {
			meta.Model = v.Str
		}
	}
	return meta
}

func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	// Replicate does not surface token usage on the wire (its billing
	// is by compute time).
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
