package cohere

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Every rerank request died here: BuildURL knew chat, embed and the model
// list, and the codec had been encoding rerank the whole time. The provider
// was never called, and the traffic row blamed it anyway.
func TestBuildURL_RerankIsAddressable(t *testing.T) {
	got, err := (&Transport{}).BuildURL(
		provcore.CallTarget{BaseURL: "https://api.cohere.com"},
		typology.WireShapeCohereRerank, false)
	if err != nil {
		t.Fatalf("BuildURL returned %v; rerank must have a URL", err)
	}
	if got != "https://api.cohere.com/v2/rerank" {
		t.Errorf("URL = %q, want https://api.cohere.com/v2/rerank", got)
	}
}

// The guard this whole class needs: whatever wire shapes the codec agrees to
// encode, the transport must be able to address. A codec that speaks a wire
// its transport cannot reach is not one adapter.
func TestBuildURL_EveryCodecShapeIsAddressable(t *testing.T) {
	tr := &Transport{}
	target := provcore.CallTarget{BaseURL: "https://api.cohere.com", ProviderModelID: "m"}
	// Each shape gets a body its own codec accepts — embed needs `input`,
	// rerank needs a query and documents. A shared stub would fail on the
	// codec's own validation and prove nothing about the transport.
	for _, tc := range []struct {
		shape typology.WireShape
		body  string
	}{
		{typology.WireShapeCohereChat, `{"model":"m","messages":[]}`},
		{typology.WireShapeCohereEmbed, `{"model":"m","input":"hello"}`},
		{typology.WireShapeCohereRerank, `{"model":"m","query":"q","documents":["d"]}`},
	} {
		_, encErr := codec{}.EncodeRequest(tc.shape, []byte(tc.body), target)
		if encErr != nil {
			t.Errorf("codec refuses %s: %v", tc.shape, encErr)
			continue
		}
		_, urlErr := tr.BuildURL(target, tc.shape, false)
		if urlErr != nil {
			t.Errorf("codec encodes %s but transport cannot address it: %v", tc.shape, urlErr)
		}
	}
}
