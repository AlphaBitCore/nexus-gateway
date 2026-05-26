package minimax

import (
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// DetectRequestMeta extracts provider/model/api-key signals from a MiniMax
// request. MiniMax authenticates via `Authorization: Bearer <token>`.
// The token is an opaque JWT issued by MiniMax — we classify it as the
// empty class and rely on the fingerprint for attribution.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "minimax"}
	if r != nil {
		meta.Path = r.URL.Path
	}

	if len(body) > 0 && gjson.ValidBytes(body) {
		if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String {
			meta.Model = m.Str
		}
	}

	if tok := traffic.ExtractBearerToken(r); tok != "" {
		// MiniMax tokens are opaque JWTs; they do not match any well-known prefix.
		meta.ApiKeyClass = ""
		meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
	}
	return meta
}

// DetectResponseUsage parses a MiniMax non-streaming chat response. MiniMax
// follows the OpenAI-compat usage shape: body.usage.{prompt_tokens,completion_tokens}.
// Older native responses surface body.base_resp usage fields; if absent
// we return parse_failed so callers know to skip token attribution.
func (a *Adapter) DetectResponseUsage(_ *http.Response, body []byte) traffic.UsageMeta {
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	if !gjson.ValidBytes(body) {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}

	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}

	var um traffic.UsageMeta
	um.Status = traffic.UsageStatusOK
	// Prefer OpenAI-compat naming.
	if p := usage.Get("prompt_tokens"); p.Exists() && p.Type == gjson.Number {
		v := int(p.Int())
		um.PromptTokens = &v
	}
	if c := usage.Get("completion_tokens"); c.Exists() && c.Type == gjson.Number {
		v := int(c.Int())
		um.CompletionTokens = &v
	}
	// MiniMax native "total_tokens" with no split — leave prompt/completion nil
	// but still report OK because we have a total.
	if um.PromptTokens == nil && um.CompletionTokens == nil {
		if t := usage.Get("total_tokens"); t.Exists() && t.Type == gjson.Number {
			v := int(t.Int())
			um.CompletionTokens = &v
		}
	}
	return um
}
