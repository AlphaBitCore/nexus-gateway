package codec

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The Gemini wire carries the model in the URL path, so the native
// differential is verbatim — the body must come back as the same slice even
// when the resolved model differs from anything in the body.
func TestGeminiRewriteNative_Verbatim(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeGeminiGenerateContent, body,
		provcore.CallTarget{ProviderModelID: "gemini-2.5-pro"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("gemini native leg must be verbatim (model lives in the URL)")
	}
}
