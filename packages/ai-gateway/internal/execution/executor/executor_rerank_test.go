package executor

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	voyage "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/voyage"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestBridgeTranslateForTarget_Rerank covers the rerank dispatch case of the
// failover-lane translator (executor_translate.go): a Cohere-shaped canonical
// rerank body routed to a Voyage target must be translated to the Voyage wire
// (top_n → top_k). Without the rerank case the translator returned nil,nil =
// "keep original body" and the Cohere body would 400 at Voyage.
func TestBridgeTranslateForTarget_Rerank(t *testing.T) {
	bridge := canonicalbridge.New(map[provcore.Format]provcore.SchemaCodec{
		provcore.FormatVoyage: voyage.NewSpec(nil).SchemaCodec,
	})
	exec := New(nil, nil, nil, bridge)

	base := provcore.Request{
		BodyFormat: provcore.FormatCohere,
		Body:       []byte(`{"model":"rerank-2","query":"find X","documents":["a","b","c"],"top_n":2}`),
	}
	ct := provcore.CallTarget{Format: provcore.FormatVoyage, ProviderModelID: "rerank-2"}

	tr, err := exec.bridgeTranslateForTarget(typology.EndpointKindRerank, base, ct)
	if err != nil {
		t.Fatalf("rerank translate Cohere→Voyage failed: %v", err)
	}
	if tr == nil {
		t.Fatal("rerank translate returned nil — the Cohere body would reach Voyage untranslated (top_n never → top_k)")
	}
	if got := gjson.GetBytes(tr.body, "top_k").Int(); got != 2 {
		t.Errorf("Voyage wire top_k = %d, want 2 (canonical top_n → Voyage top_k)", got)
	}
	if gjson.GetBytes(tr.body, "top_n").Exists() {
		t.Error("Voyage wire must not carry the canonical field name top_n")
	}
	if got := gjson.GetBytes(tr.body, "documents.#").Int(); got != 3 {
		t.Errorf("Voyage wire documents count = %d, want 3", got)
	}
}

// TestBridgeTranslateForTarget_RerankValidationBindsOnFailover proves the
// canonical validation runs on the failover lane: an invalid rerank body
// (empty query) fails translation with the field-naming error rather than
// silently forwarding a bad body downstream.
func TestBridgeTranslateForTarget_RerankValidationBindsOnFailover(t *testing.T) {
	bridge := canonicalbridge.New(map[provcore.Format]provcore.SchemaCodec{
		provcore.FormatVoyage: voyage.NewSpec(nil).SchemaCodec,
	})
	exec := New(nil, nil, nil, bridge)

	base := provcore.Request{
		BodyFormat: provcore.FormatCohere,
		Body:       []byte(`{"model":"rerank-2","query":"","documents":["a"]}`),
	}
	ct := provcore.CallTarget{Format: provcore.FormatVoyage, ProviderModelID: "rerank-2"}

	if _, err := exec.bridgeTranslateForTarget(typology.EndpointKindRerank, base, ct); err == nil {
		t.Fatal("empty query must fail rerank translation on the failover lane")
	}
}
