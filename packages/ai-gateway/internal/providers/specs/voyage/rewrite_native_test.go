package voyage

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// No evidence-cited native quirk exists on the Voyage embeddings wire; the
// differential is verbatim until an observed vendor rejection says otherwise.
func TestVoyageRewriteNative_Verbatim(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":["hi"]}`)
	res, err := codec{}.RewriteNative(typology.WireShapeVoyageEmbeddings, body,
		provcore.CallTarget{ProviderModelID: "voyage-3"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("voyage native leg must be verbatim today")
	}
}
