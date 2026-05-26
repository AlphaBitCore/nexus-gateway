package anthropic

import (
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// DetectRequestMeta extracts provider/model/api-key signals from an
// Anthropic Messages API request. Anthropic uses `x-api-key` primarily,
// but some clients send `Authorization: Bearer` — both are supported.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "anthropic"}
	if r != nil {
		meta.Path = r.URL.Path
	}

	if len(body) > 0 && gjson.ValidBytes(body) {
		if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String {
			meta.Model = m.Str
		}
	}

	key := ""
	if r != nil {
		if v := r.Header.Get("x-api-key"); v != "" {
			key = v
		}
	}
	if key == "" {
		key = traffic.ExtractBearerToken(r)
	}
	if key != "" {
		meta.ApiKeyClass = traffic.ApiKeyClassify(key)
		meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(key)
	}
	return meta
}

// DetectResponseUsage parses an Anthropic non-streaming Messages response
// for usage.input_tokens / usage.output_tokens.
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
	if p := usage.Get("input_tokens"); p.Exists() && p.Type == gjson.Number {
		v := int(p.Int())
		um.PromptTokens = &v
	}
	if c := usage.Get("output_tokens"); c.Exists() && c.Type == gjson.Number {
		v := int(c.Int())
		um.CompletionTokens = &v
	}
	// Prompt-cache fields: cache_read_input_tokens = tokens served from cache
	// (CacheReadTokens); cache_creation_input_tokens = tokens written to cache
	// this request (CacheCreationTokens); needed for net-savings cost model.
	if v := usage.Get("cache_read_input_tokens"); v.Exists() && v.Type == gjson.Number {
		n := int(v.Int())
		um.CacheReadTokens = &n
	}
	if v := usage.Get("cache_creation_input_tokens"); v.Exists() && v.Type == gjson.Number {
		n := int(v.Int())
		um.CacheCreationTokens = &n
	}
	return um
}
