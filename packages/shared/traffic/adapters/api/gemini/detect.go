package gemini

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// DetectRequestMeta extracts provider/model/api-key signals from a Gemini
// generateContent request. Gemini embeds the model in the URL path
// (`/v1beta/models/<model>:generateContent`) and authenticates via
// `x-goog-api-key` header or `?key=` query param.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "gemini"}
	if r != nil {
		meta.Path = r.URL.Path
		meta.Model = modelFromGeminiPath(r.URL.Path)
	}

	if meta.Model == "" && len(body) > 0 && gjson.ValidBytes(body) {
		if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String {
			meta.Model = m.Str
		}
	}

	key := ""
	if r != nil {
		if v := r.Header.Get("x-goog-api-key"); v != "" {
			key = v
		} else if v := r.URL.Query().Get("key"); v != "" {
			key = v
		} else {
			key = traffic.ExtractBearerToken(r)
		}
	}
	if key != "" {
		meta.ApiKeyClass = traffic.ApiKeyClassify(key)
		meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(key)
	}
	return meta
}

// DetectResponseUsage parses a Gemini generateContent non-streaming response.
// Gemini returns usageMetadata with promptTokenCount and candidatesTokenCount.
func (a *Adapter) DetectResponseUsage(_ *http.Response, body []byte) traffic.UsageMeta {
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	if !gjson.ValidBytes(body) {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}

	usage := gjson.GetBytes(body, "usageMetadata")
	if !usage.Exists() {
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}

	var um traffic.UsageMeta
	um.Status = traffic.UsageStatusOK
	if p := usage.Get("promptTokenCount"); p.Exists() && p.Type == gjson.Number {
		v := int(p.Int())
		um.PromptTokens = &v
	}
	if c := usage.Get("candidatesTokenCount"); c.Exists() && c.Type == gjson.Number {
		v := int(c.Int())
		um.CompletionTokens = &v
	}
	// Gemini context-cache + thinking-tokens splits, mirroring what
	// spec_gemini.codec.DecodeResponse writes to providers.Usage.
	// Doc: https://ai.google.dev/api/generate-content#UsageMetadata
	if v := usage.Get("cachedContentTokenCount"); v.Exists() && v.Type == gjson.Number {
		n := int(v.Int())
		um.CacheReadTokens = &n
	}
	if v := usage.Get("thoughtsTokenCount"); v.Exists() && v.Type == gjson.Number {
		n := int(v.Int())
		um.ReasoningTokens = &n
	}
	return um
}

// modelFromGeminiPath extracts "gemini-1.5-pro" from
// "/v1beta/models/gemini-1.5-pro:generateContent".
// Returns "" when the path does not match the pattern.
func modelFromGeminiPath(path string) string {
	const marker = "/models/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		return rest[:colon]
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return rest[:slash]
	}
	return rest
}
