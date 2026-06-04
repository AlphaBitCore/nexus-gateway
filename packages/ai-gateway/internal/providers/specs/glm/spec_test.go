package glm_test

import (
	"log/slog"
	"net/http"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/glm"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestGLM_BuildURL(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://open.bigmodel.cn"}, typology.WireShapeOpenAIChat, false)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got != "https://open.bigmodel.cn/api/paas/v4/chat/completions" {
		t.Errorf("got %q", got)
	}
}

func TestGLM_ApplyAuth_BearerJWT(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "jwt"}); err != nil {
		t.Fatalf("%v", err)
	}
	if req.Header.Get("Authorization") != "Bearer jwt" {
		t.Errorf("Authorization got %q", req.Header.Get("Authorization"))
	}
}
