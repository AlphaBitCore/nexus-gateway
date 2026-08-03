package cohere

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// No evidence-cited native quirk exists on the Cohere wire; the differential
// is verbatim until an observed vendor rejection says otherwise.
func TestCohereRewriteNative_Verbatim(t *testing.T) {
	body := []byte(`{"model":"command-a-03-2025","messages":[{"role":"user","content":"hi"}]}`)
	res, err := codec{}.RewriteNative(typology.WireShapeCohereChat, body,
		provcore.CallTarget{ProviderModelID: "command-a-03-2025"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("cohere native leg must be verbatim today")
	}
}
