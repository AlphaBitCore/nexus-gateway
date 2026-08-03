package codec

// Error and fallback arms of the contract doors. Named failure modes: a
// truncated body whose due field gjson still sees must fall through the
// surgical door and surface the decode error (never a partial edit); a
// non-object body handed directly to a stamping arm errors instead of
// fabricating a synthetic object; duplicated keys on the responses and
// embeddings wires take the decode door and still land their edits.

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func TestApplyBytes_TruncatedBody_SurgicalEditStillLands(t *testing.T) {
	// sjson tolerates a truncated object (verified: DeleteBytes succeeds
	// and removes the field), so the surgical door edits what it can and
	// the otherwise-unparseable bytes forward as sent — the upstream is
	// the validator (wire must equal audit), matching the direction the
	// non-object dispatch carve-out already took.
	s := newRuleSet(testRules())
	body := []byte(`{"model":"quirk-1","temperature":0`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok {
		t.Fatal("sjson handles this body; the surgical door must not bounce it")
	}
	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("the due edit must land: %s", out)
	}
	if len(rw) != 1 || rw[0] != "temperature→removed" {
		t.Fatalf("rewrites: %v", rw)
	}
}

func TestRewriteNative_ChatTruncatedQuirkBody_ForwardsEdited(t *testing.T) {
	// End-to-end pin of the same behaviour: no local 400 — the edited,
	// still-truncated body forwards and the upstream reports its own
	// parse error. (The legacy quirk map path 400'd locally here; the
	// surgical door aligns the quirk class with the non-quirk class,
	// which always forwarded such bodies.)
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"quirk-1","temperature":0`),
		provcore.CallTarget{ProviderModelID: "quirk-1"}, false)
	if err != nil {
		t.Fatalf("truncated body must forward, not 400 locally: %v", err)
	}
	if gjson.GetBytes(res.Body, "temperature").Exists() {
		t.Fatalf("the strip must land: %s", res.Body)
	}
}

func TestApplyMap_RenameWithoutTargetPresent_SetsValue(t *testing.T) {
	s := newRuleSet(testRules())
	payload := map[string]any{"max_tokens": float64(64)}
	rw := s.applyMap(payload, "quirk-1")
	if payload["max_completion_tokens"] != float64(64) {
		t.Fatalf("rename must carry the value when the target is absent: %v", payload)
	}
	if len(rw) != 1 || rw[0] != "max_tokens→max_completion_tokens" {
		t.Fatalf("rewrites: %v", rw)
	}
}

func TestChatDifferential_NonObjectBody_Errors(t *testing.T) {
	// Dispatch's non-object carve-out keeps such bodies away from the
	// codec on the native leg; a direct canonical-door call must error
	// rather than fabricate {"model":X} around a non-object.
	_, err := plain().EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`["not","an","object"]`), provcore.CallTarget{ProviderModelID: "gpt-4o"})
	if err == nil {
		t.Fatal("stamping a non-object body must error")
	}
}

func TestRewriteNative_CompletionsLegacy_UnstampableBody_Errors(t *testing.T) {
	// Duplicated model keys force the stamp's decode arm; a body that
	// cannot decode must error.
	_, err := plain().RewriteNative(typology.WireShapeOpenAICompletionsLegacy,
		[]byte(`{"model":"a","model":"b","prompt":`),
		provcore.CallTarget{ProviderModelID: "gpt-3.5-turbo-instruct"}, false)
	if err == nil {
		t.Fatal("unstampable legacy body must error")
	}
}

func TestEncodeResponsesNative_UnstampableBody_Errors(t *testing.T) {
	_, err := quirky().RewriteNative(typology.WireShapeOpenAIResponses,
		[]byte(`{"model":"a","model":"b","input":`),
		provcore.CallTarget{ProviderModelID: "quirk-9"}, false)
	if err == nil {
		t.Fatal("unstampable responses body must error")
	}
}

func TestEncodeResponsesNative_DupQuirkKey_MapDoorStrips(t *testing.T) {
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIResponses,
		[]byte(`{"model":"quirk-9","input":"x","temperature":0.1,"temperature":0.9}`),
		provcore.CallTarget{ProviderModelID: "quirk-9"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "temperature").Exists() {
		t.Fatalf("decode door must strip the deduplicated field: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "temperature→removed" {
		t.Fatalf("strip must be reported: %v", res.Rewrites)
	}
}

func TestRewriteNative_Embeddings_UnstampableBody_Errors(t *testing.T) {
	_, err := plain().RewriteNative(typology.WireShapeOpenAIEmbeddings,
		[]byte(`["bare","array"]`), provcore.CallTarget{ProviderModelID: "text-embedding-3-small"}, false)
	if err == nil {
		t.Fatal("stamping a non-object embeddings body must error")
	}
}

func TestRewriteNative_Embeddings_DupQuirkKey_MapDoorStrips(t *testing.T) {
	c := newIdentity(Contract{
		Embeddings: []FieldRule{{Applies: quirkGate, Field: "dimensions"}},
	})
	res, err := c.RewriteNative(typology.WireShapeOpenAIEmbeddings,
		[]byte(`{"model":"quirk-embed","input":"x","dimensions":16,"dimensions":32}`),
		provcore.CallTarget{ProviderModelID: "quirk-embed"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "dimensions").Exists() {
		t.Fatalf("decode door must strip the deduplicated field: %s", res.Body)
	}
}

func TestEncodeEmbeddings_DupQuirkKey_MapDoorStrips(t *testing.T) {
	c := newIdentity(Contract{
		Embeddings: []FieldRule{{Applies: quirkGate, Field: "dimensions"}},
	})
	res, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings,
		[]byte(`{"model":"quirk-embed","input":"x","dimensions":16,"dimensions":32}`),
		provcore.CallTarget{ProviderModelID: "quirk-embed"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "dimensions").Exists() {
		t.Fatalf("canonical-door decode fallback must strip too: %s", res.Body)
	}
}

func TestEncodeEmbeddings_NonObjectValidJSON_Errors(t *testing.T) {
	// A valid-JSON array passes the ValidBytes guard but cannot carry a
	// model stamp; the canonical door errors (its wire contract is an
	// object) instead of forwarding a body the upstream rejects anyway.
	_, err := plain().EncodeRequest(typology.WireShapeOpenAIEmbeddings,
		[]byte(`["x"]`), provcore.CallTarget{ProviderModelID: "text-embedding-3-small"})
	if err == nil {
		t.Fatal("non-object embeddings body must error on the canonical door")
	}
}
