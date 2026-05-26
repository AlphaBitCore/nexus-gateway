package azure

import (
	"net/http"
	"regexp"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// azureDeploymentNamePattern captures the deployment (= model alias) from
// Azure OpenAI URLs like `/openai/deployments/<name>/chat/completions`.
var azureDeploymentNamePattern = regexp.MustCompile(`/openai/deployments/([^/]+)/`)

// DetectRequestMeta extracts provider/model/api-key signals from an Azure
// OpenAI request. Azure uses `api-key` header for auth, or occasionally
// `Authorization: Bearer` for Entra ID (opaque AAD token).
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "azure-openai"}
	if r != nil {
		meta.Path = r.URL.Path
		meta.Model = modelFromAzurePath(r.URL.Path)
	}

	if meta.Model == "" && len(body) > 0 && gjson.ValidBytes(body) {
		if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String {
			meta.Model = m.Str
		}
	}

	if r != nil {
		if k := r.Header.Get("api-key"); k != "" {
			meta.ApiKeyClass = "azure-api-key"
			meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(k)
		} else if tok := traffic.ExtractBearerToken(r); tok != "" {
			// Entra ID / AAD Bearer token — opaque JWT from AAD.
			meta.ApiKeyClass = "azure-api-key"
			meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
		}
	}
	return meta
}

// DetectResponseUsage delegates to the inner OpenAI detect implementation;
// Azure and OpenAI share the same response schema for chat/completions and
// embeddings. (Re-parsing is cheap; no need for type-assertion ceremony.)
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}

// modelFromAzurePath returns the deployment name from an Azure OpenAI path,
// or "" if the path does not match.
func modelFromAzurePath(path string) string {
	m := azureDeploymentNamePattern.FindStringSubmatch(path)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
