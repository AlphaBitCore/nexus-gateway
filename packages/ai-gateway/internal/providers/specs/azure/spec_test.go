package azure_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/azure"
)

func TestAzure_BuildURL_UsesDeploymentAndVersion(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	tgt := provcore.CallTarget{
		BaseURL: "https://my-resource.openai.azure.com",
		Extras: map[string]string{
			"azure.deployment": "gpt4o",
			"azure.apiVersion": "2024-10-21",
		},
	}
	got, err := tr.BuildURL(tgt, typology.WireShapeOpenAIChat, false)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(got, "/openai/deployments/gpt4o/chat/completions") {
		t.Errorf("deployment path: %q", got)
	}
	if !strings.Contains(got, "api-version=2024-10-21") {
		t.Errorf("api-version missing: %q", got)
	}
}

// TestAzure_DefaultAPIVersionIsLatestGA pins the audit decision: when
// the resolver does not supply azure.apiVersion explicitly, the URL
// carries the latest Azure GA version (2024-10-21 at audit time).
// Older defaults silently miss structured-outputs / o1 reasoning fields.
func TestAzure_DefaultAPIVersionIsLatestGA(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL: "https://r.openai.azure.com",
			Extras:  map[string]string{"azure.deployment": "gpt4o"},
		},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(got, "api-version=2024-10-21") {
		t.Errorf("default api-version stale: %q", got)
	}
}

func TestAzure_ApplyAuth_ApiKey(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "k"}); err != nil {
		t.Fatalf("%v", err)
	}
	if req.Header.Get("api-key") != "k" {
		t.Errorf("api-key header missing")
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Authorization must not leak")
	}
}
