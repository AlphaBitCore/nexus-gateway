package generic

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// DetectRequestMeta returns an empty-provider signal. The generic adapter
// is configured per domain with JSONPath expressions for content
// extraction — it has no structural assumptions about where provider,
// model, or usage fields live. Request-side detection therefore only
// classifies the api key (if any) so the row still carries attribution.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{}
	if r != nil {
		meta.Path = r.URL.Path
	}

	if tok := traffic.ExtractBearerToken(r); tok != "" {
		meta.ApiKeyClass = traffic.ApiKeyClassify(tok)
		meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
	}
	return meta
}

// DetectResponseUsage for the generic adapter always reports non_llm:
// without a known schema we cannot locate usage fields, and guessing is
// worse than being explicit about the gap.
func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}
