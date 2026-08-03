package bedrock

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The Bedrock wire encodes the model into the invoke URL, so the native
// differential is verbatim.
func TestBedrockRewriteNative_Verbatim(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
	res, err := codec{}.RewriteNative(typology.WireShapeBedrockConverse, body,
		provcore.CallTarget{ProviderModelID: "anthropic.claude-opus-4-8"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("bedrock native leg must be verbatim (model lives in the URL)")
	}
}
