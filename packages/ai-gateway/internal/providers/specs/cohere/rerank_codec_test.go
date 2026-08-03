// Package cohere_test — rerank codec tests for the Cohere SchemaCodec.
//
// The canonical rerank shape IS the Cohere shape, so both encode and decode
// are near-identity. These tests exercise the WireShapeCohereRerank dispatch
// in codec.go plus the encodeCohereRerank / decodeCohereRerankResponse legs:
//   - encode: model injection from CallTarget, verbatim passthrough when the
//     body already carries a model, and the invalid/empty-body failure modes.
//   - decode: identity round-trip that must preserve
//     meta.billed_units.search_units verbatim (the cost stamper re-parses it
//     off the canonical body) while leaving DecodeResult.Usage empty (rerank
//     reports no token usage on the Cohere wire).
package cohere_test

import (
	"bytes"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// EncodeRequest rerank

func TestCohereCodec_EncodeRequest_rerank_emptyBody_returnsError(t *testing.T) {
	c := newCohereCodec()
	_, err := c.EncodeRequest(typology.WireShapeCohereRerank, nil, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for empty rerank body")
	}
}

func TestCohereCodec_EncodeRequest_rerank_invalidJSON_returnsError(t *testing.T) {
	c := newCohereCodec()
	_, err := c.EncodeRequest(typology.WireShapeCohereRerank, []byte(`{not json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for invalid rerank JSON")
	}
}

func TestCohereCodec_EncodeRequest_rerank_modelInjectedFromTarget(t *testing.T) {
	// Body omits `model`; the resolved target provides it → codec injects it so
	// Cohere sees the provider model name.
	c := newCohereCodec()
	body := []byte(`{"query":"what is nexus","documents":["a","b"],"top_n":2}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereRerank, body, provcore.CallTarget{ProviderModelID: "rerank-english-v3.0"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got := gjson.GetBytes(encRes.Body, "model").Str; got != "rerank-english-v3.0" {
		t.Errorf("model = %q, want injected rerank-english-v3.0; wire=%s", got, encRes.Body)
	}
	// The other canonical fields must survive the injection round-trip.
	if got := gjson.GetBytes(encRes.Body, "query").Str; got != "what is nexus" {
		t.Errorf("query lost during model injection: %s", encRes.Body)
	}
	if n := gjson.GetBytes(encRes.Body, "documents.#").Int(); n != 2 {
		t.Errorf("documents lost during model injection: %s", encRes.Body)
	}
	if got := gjson.GetBytes(encRes.Body, "top_n").Int(); got != 2 {
		t.Errorf("top_n lost during model injection: %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_rerank_bodyWithModel_passthroughUnchanged(t *testing.T) {
	// Body already carries `model` → verbatim passthrough (canonical IS Cohere
	// shape), even when the target also names a model.
	c := newCohereCodec()
	body := []byte(`{"model":"rerank-english-v3.0","query":"q","documents":["a","b"],"top_n":1}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereRerank, body, provcore.CallTarget{ProviderModelID: "rerank-multilingual-v3.0"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !bytes.Equal(encRes.Body, body) {
		t.Errorf("body must pass through byte-for-byte: got %s, want %s", encRes.Body, body)
	}
	// The target model must NOT override the body model on the passthrough path.
	if got := gjson.GetBytes(encRes.Body, "model").Str; got != "rerank-english-v3.0" {
		t.Errorf("body model must win: got %q", got)
	}
}

func TestCohereCodec_EncodeRequest_rerank_nonObjectBody_modelInjection_returnsError(t *testing.T) {
	// A valid-JSON but non-object body (a bare array) passes gjson.ValidBytes and
	// has no `model`, so the model-injection path runs and json.Unmarshal into a
	// map[string]any fails — the codec must surface that error, not panic.
	c := newCohereCodec()
	_, err := c.EncodeRequest(typology.WireShapeCohereRerank, []byte(`[1,2,3]`), provcore.CallTarget{ProviderModelID: "rerank-english-v3.0"})
	if err == nil {
		t.Fatal("expected error unmarshalling a non-object rerank body during model injection")
	}
}

func TestCohereCodec_EncodeRequest_rerank_contentType(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{"model":"rerank-english-v3.0","query":"q","documents":["a"]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereRerank, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.ContentType != "application/json" {
		t.Errorf("ContentType: got %q, want application/json", encRes.ContentType)
	}
}

// DecodeResponse rerank

func TestCohereCodec_DecodeResponse_rerank_emptyBody_identity(t *testing.T) {
	c := newCohereCodec()
	decRes, err := c.DecodeResponse(typology.WireShapeCohereRerank, []byte{}, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(decRes.CanonicalBody) != 0 {
		t.Errorf("empty body must pass through empty: %s", decRes.CanonicalBody)
	}
}

func TestCohereCodec_DecodeResponse_rerank_invalidJSON_returnsError(t *testing.T) {
	c := newCohereCodec()
	_, err := c.DecodeResponse(typology.WireShapeCohereRerank, []byte(`{not json`), "application/json", provcore.DecodeContext{})
	if err == nil {
		t.Fatal("expected error for invalid rerank response JSON")
	}
}

// TestCohereCodec_DecodeResponse_rerank_identity_preservesSearchUnits pins the
// cost contract: the Cohere response is returned verbatim as the canonical body
// so meta.billed_units.search_units survives for the cost stamper to re-parse,
// and DecodeResult.Usage stays empty (rerank reports no token usage).
func TestCohereCodec_DecodeResponse_rerank_identity_preservesSearchUnits(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{
		"id":"rr-123",
		"results":[
			{"index":1,"relevance_score":0.98},
			{"index":0,"relevance_score":0.42}
		],
		"meta":{"billed_units":{"search_units":1}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereRerank, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	// Identity: the canonical body is the Cohere body verbatim.
	if !bytes.Equal(decRes.CanonicalBody, body) {
		t.Errorf("decode must be identity: got %s", decRes.CanonicalBody)
	}
	// search_units must survive — the cost stamper reads it off this body.
	if su := gjson.GetBytes(decRes.CanonicalBody, "meta.billed_units.search_units").Int(); su != 1 {
		t.Errorf("search_units = %d, want 1 (cost stamper input); body=%s", su, decRes.CanonicalBody)
	}
	// results[] ordering/scores must survive verbatim.
	if got := gjson.GetBytes(decRes.CanonicalBody, "results.0.relevance_score").Float(); got != 0.98 {
		t.Errorf("results[0].relevance_score = %v, want 0.98", got)
	}
	if got := gjson.GetBytes(decRes.CanonicalBody, "results.0.index").Int(); got != 1 {
		t.Errorf("results[0].index = %d, want 1", got)
	}
	// Rerank reports no token usage on the Cohere wire — Usage stays empty.
	if decRes.Usage.PromptTokens != nil || decRes.Usage.CompletionTokens != nil || decRes.Usage.TotalTokens != nil {
		t.Errorf("Usage must be empty for Cohere rerank, got %+v", decRes.Usage)
	}
}
