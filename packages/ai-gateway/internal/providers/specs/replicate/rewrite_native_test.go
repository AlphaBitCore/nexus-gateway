package replicate

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The Replicate wire carries the model in the URL path (owner/name), so the
// native differential is verbatim.
func TestReplicateRewriteNative_Verbatim(t *testing.T) {
	body := []byte(`{"input":{"prompt":"hi"}}`)
	res, err := codec{}.RewriteNative(typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "anthropic/claude-4.5-sonnet"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("replicate native leg must be verbatim (model lives in the URL)")
	}
}
