package glm

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_DelegatesToOpenAI(t *testing.T) {
	body := []byte(`{"model":"glm-4","messages":[{"role":"user","content":"email a@b.com"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/api/paas/v4/chat/completions",
		traffic.NormalizedContent{Segments: []string{"email [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "email [REDACTED]" {
		t.Errorf("content = %q", got)
	}
}
