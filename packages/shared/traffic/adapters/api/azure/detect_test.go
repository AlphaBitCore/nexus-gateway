package azure

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://contoso.openai.azure.com/openai/deployments/gpt4-prod/chat/completions?api-version=2024-02-01",
		nil)
	r.Header.Set("api-key", "aabbccddeeff")

	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "azure-openai" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "gpt4-prod" {
		t.Errorf("model = %q, want gpt4-prod", got.Model)
	}
	if got.ApiKeyClass != "azure-api-key" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint != traffic.ApiKeyFingerprint("aabbccddeeff") {
		t.Errorf("fingerprint mismatch")
	}
}

func TestDetectRequestMetaEntraBearer(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://contoso.openai.azure.com/openai/deployments/gpt4/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.aad.token")

	got := a.DetectRequestMeta(r, nil)
	if got.ApiKeyClass != "azure-api-key" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectResponseUsage(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":11,"completion_tokens":22}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 11 || *um.CompletionTokens != 22 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}

// TestDetectRequestMeta_NilRequest pins that a nil http.Request degrades
// gracefully — provider is still stamped, path/model/auth all stay empty.
// Hub/CP callers occasionally invoke meta extraction on bodies they
// captured separately from the request, so the nil-request branch must
// not panic.
func TestDetectRequestMeta_NilRequest(t *testing.T) {
	a := &Adapter{}
	got := a.DetectRequestMeta(nil, nil)
	if got.Provider != "azure-openai" {
		t.Errorf("provider = %q, want azure-openai", got.Provider)
	}
	if got.Model != "" {
		t.Errorf("model = %q, want empty", got.Model)
	}
	if got.Path != "" {
		t.Errorf("path = %q, want empty", got.Path)
	}
	if got.ApiKeyClass != "" {
		t.Errorf("class = %q, want empty", got.ApiKeyClass)
	}
}

// TestDetectRequestMeta_ModelFromBody pins the body fallback: when the
// path doesn't carry a deployment segment (non-Azure-style URL), the
// adapter must fall back to the JSON body's "model" field. This matches
// Azure's deployment-name = model-alias contract — without the fallback
// any custom-route Azure caller would have model = "" in the audit.
func TestDetectRequestMeta_ModelFromBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://contoso.openai.azure.com/some/custom/path",
		nil)
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	got := a.DetectRequestMeta(r, body)
	if got.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini (body fallback)", got.Model)
	}
}

// TestDetectRequestMeta_NoAuthHeaders pins that requests with neither
// api-key nor Authorization headers leave ApiKeyClass empty rather than
// stamping a misleading class. Production sees this on test pings and
// preflight CORS-style probes; the audit must not pretend an auth class
// was present.
func TestDetectRequestMeta_NoAuthHeaders(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://contoso.openai.azure.com/openai/deployments/gpt4/chat/completions",
		nil)
	got := a.DetectRequestMeta(r, nil)
	if got.ApiKeyClass != "" {
		t.Errorf("class = %q, want empty when no auth headers present", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint != "" {
		t.Errorf("fingerprint = %q, want empty", got.ApiKeyFingerprint)
	}
	if got.Model != "gpt4" {
		t.Errorf("model = %q, want gpt4 from path", got.Model)
	}
}

// TestModelFromAzurePath_NoMatch pins the empty-string return when the
// path is not an Azure deployment URL — the regex submatch length check
// is the gate that prevents a panic on a path like "/v1/chat/completions".
func TestModelFromAzurePath_NoMatch(t *testing.T) {
	if got := modelFromAzurePath("/v1/chat/completions"); got != "" {
		t.Errorf("modelFromAzurePath = %q, want empty for non-Azure path", got)
	}
	if got := modelFromAzurePath(""); got != "" {
		t.Errorf("modelFromAzurePath(empty) = %q, want empty", got)
	}
}
