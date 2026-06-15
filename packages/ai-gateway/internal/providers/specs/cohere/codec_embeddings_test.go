// Package cohere_test — embedding codec tests for the Cohere SchemaCodec.
// Named failure modes per provider-adapter-architecture.md §3a:
//   - Rule 3: per-model wire quirks owned by the adapter
//   - Rule 7: source comments with empirical 400 citations
package cohere_test

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/cohere"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func newCohereCodec() provcore.SchemaCodec {
	spec := cohere.NewSpec(nil)
	return spec.SchemaCodec
}

// EncodeRequest embeddings

func TestCohereCodec_EncodeRequest_embeddings_string_wrapsToTextsArray(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hello world"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	texts := gjson.GetBytes(encRes.Body, "texts")
	if !texts.IsArray() {
		t.Fatalf("texts must be an array, got: %s", encRes.Body)
	}
	arr := texts.Array()
	if len(arr) != 1 || arr[0].Str != "hello world" {
		t.Errorf("texts: got %s, want [\"hello world\"]", encRes.Body)
	}
	if gjson.GetBytes(encRes.Body, "model").Str != "embed-english-light-v2.0" {
		t.Errorf("model missing: %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_stringArray_wrapsToTextsArray(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":["a","b","c"]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	texts := gjson.GetBytes(encRes.Body, "texts")
	if !texts.IsArray() || len(texts.Array()) != 3 {
		t.Fatalf("texts: got %s, want 3 elements", encRes.Body)
	}
	arr := texts.Array()
	if arr[0].Str != "a" || arr[1].Str != "b" || arr[2].Str != "c" {
		t.Errorf("texts content: got %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_tokenArray_returns400(t *testing.T) {
	// Token arrays ([]int) are unsupported by Cohere.
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":[1,2,3,4]}`)
	_, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for token array input")
	}
	if !strings.Contains(err.Error(), "token_array_unsupported_by_cohere") {
		t.Errorf("error should mention token_array_unsupported_by_cohere: %v", err)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_tokenBatchArray_returns400(t *testing.T) {
	// Batch token arrays ([[int],...]) are unsupported by Cohere.
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":[[1,2],[3,4]]}`)
	_, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for batch token array input")
	}
	if !strings.Contains(err.Error(), "token_array_unsupported_by_cohere") {
		t.Errorf("error should mention token_array_unsupported_by_cohere: %v", err)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_v3Model_missingInputType_defaultsSearchDocument(t *testing.T) {
	// Cohere v3 models require input_type on the wire. When the caller omits
	// nexus.ext.cohere.input_type the codec defaults to "search_document"
	// rather than rejecting — matching the Bedrock-Cohere codec and avoiding
	// the filter/codec disagreement where the capability filter admits a
	// request the codec then 400s (audit F-0216).
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-v3.0","input":"hello"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{ProviderModelID: "embed-english-v3.0"})
	if err != nil {
		t.Fatalf("EncodeRequest should not error for v3 without input_type: %v", err)
	}
	if got := gjson.GetBytes(encRes.Body, "input_type").Str; got != "search_document" {
		t.Errorf("input_type = %q, want defaulted %q; wire=%s", got, "search_document", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_v3Model_withInputType_ok(t *testing.T) {
	// v3 model with input_type extension supplied.
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-v3.0","input":"hello","nexus":{"ext":{"cohere":{"input_type":"search_document"}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{ProviderModelID: "embed-english-v3.0"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "input_type").Str != "search_document" {
		t.Errorf("input_type missing from wire body: %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_multilingualV3_defaultsInputType(t *testing.T) {
	// embed-multilingual-v3.0 also requires input_type on the wire; the codec
	// defaults it to "search_document" when omitted (audit F-0216).
	c := newCohereCodec()
	body := []byte(`{"model":"embed-multilingual-v3.0","input":"hello"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{ProviderModelID: "embed-multilingual-v3.0"})
	if err != nil {
		t.Fatalf("EncodeRequest should not error for multilingual v3 without input_type: %v", err)
	}
	if got := gjson.GetBytes(encRes.Body, "input_type").Str; got != "search_document" {
		t.Errorf("input_type = %q, want defaulted %q; wire=%s", got, "search_document", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_base64EncodingFormat_returns400(t *testing.T) {
	// base64 encoding_format has no Cohere wire equivalent.
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hello","encoding_format":"base64"}`)
	_, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for base64 encoding_format")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("error should mention base64: %v", err)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_floatEncodingFormat_passesThrough(t *testing.T) {
	// float encoding_format → embedding_types: ["float"]
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hello","encoding_format":"float"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	etypes := gjson.GetBytes(encRes.Body, "embedding_types")
	if !etypes.IsArray() {
		t.Fatalf("embedding_types must be array for float: %s", encRes.Body)
	}
	if etypes.Array()[0].Str != "float" {
		t.Errorf("embedding_types: got %s, want [\"float\"]", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_extEmbeddingTypes_overridesEncFormat(t *testing.T) {
	// nexus.ext.cohere.embedding_types overrides encoding_format derivation.
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hello","encoding_format":"float","nexus":{"ext":{"cohere":{"embedding_types":["float","int8"]}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	etypes := gjson.GetBytes(encRes.Body, "embedding_types")
	if !etypes.IsArray() || len(etypes.Array()) != 2 {
		t.Fatalf("embedding_types should be 2-element array: %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_truncateExtension_passesThrough(t *testing.T) {
	// nexus.ext.cohere.truncate → wire truncate
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hello","nexus":{"ext":{"cohere":{"truncate":"START"}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "truncate").Str != "START" {
		t.Errorf("truncate: got %s, want START", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_truncateDefault_isEND(t *testing.T) {
	// Default truncate is END.
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hello"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "truncate").Str != "END" {
		t.Errorf("truncate default: got %s, want END", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_dimensionsIgnored(t *testing.T) {
	// Cohere models are fixed-dimension; dimensions field is ignored (not
	// forwarded to the wire — Cohere rejects unknown fields).
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hello","dimensions":512}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// dimensions must NOT appear in the wire body.
	if gjson.GetBytes(encRes.Body, "dimensions").Exists() {
		t.Errorf("dimensions must not be forwarded to Cohere: %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_invalidJSON_returns400(t *testing.T) {
	c := newCohereCodec()
	_, err := c.EncodeRequest(typology.WireShapeCohereEmbed, []byte(`{not json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCohereCodec_EncodeRequest_embeddings_emptyBody_ok(t *testing.T) {
	c := newCohereCodec()
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, nil, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("empty body: %v", err)
	}
	if len(encRes.Body) != 0 {
		t.Errorf("expected empty result: %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_modelFromTarget(t *testing.T) {
	// Model from target.ProviderModelID when body has no model field.
	c := newCohereCodec()
	body := []byte(`{"input":"hello"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{ProviderModelID: "embed-english-light-v2.0"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "model").Str != "embed-english-light-v2.0" {
		t.Errorf("model from target: got %s", encRes.Body)
	}
}

func TestCohereCodec_EncodeRequest_embeddings_contentType(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{"model":"embed-english-light-v2.0","input":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.ContentType != "application/json" {
		t.Errorf("ContentType: got %q, want application/json", encRes.ContentType)
	}
}

// DecodeResponse embeddings

func TestCohereCodec_DecodeResponse_embeddings_flatArray(t *testing.T) {
	// Cohere flat embeddings array → canonical data[]
	c := newCohereCodec()
	body := []byte(`{
		"id":"emb-123",
		"response_type":"embeddings_floats",
		"embeddings":[[0.1,0.2,0.3],[0.4,0.5,0.6]],
		"texts":["a","b"],
		"meta":{"billed_units":{"input_tokens":10}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	data := gjson.GetBytes(decRes.CanonicalBody, "data")
	if !data.IsArray() || len(data.Array()) != 2 {
		t.Fatalf("data must be 2-element array: %s", decRes.CanonicalBody)
	}
	emb0 := data.Array()[0].Get("embedding")
	if !emb0.IsArray() || len(emb0.Array()) != 3 {
		t.Errorf("embedding[0] must be 3-element array: %s", decRes.CanonicalBody)
	}
	if emb0.Array()[0].Float() != 0.1 {
		t.Errorf("embedding[0][0]: got %f, want 0.1", emb0.Array()[0].Float())
	}
	if gjson.GetBytes(decRes.CanonicalBody, "object").Str != "list" {
		t.Errorf("object must be 'list': %s", decRes.CanonicalBody)
	}
}

// TestCohereCodec_DecodeResponse_embeddings_countMismatch_rejected pins
// F-0220: a response with fewer vectors than the request `texts` must fail
// the decode (→ 502) instead of returning misaligned vectors.
func TestCohereCodec_DecodeResponse_embeddings_countMismatch_rejected(t *testing.T) {
	c := newCohereCodec()
	reqBody := []byte(`{"model":"embed-english-v3.0","texts":["a","b","c"]}`)
	body := []byte(`{"id":"emb","embeddings":[[0.1],[0.2]],"texts":["a","b"]}`)
	_, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json",
		provcore.DecodeContext{RequestBody: reqBody})
	if err == nil || !strings.Contains(err.Error(), "embedding count mismatch") {
		t.Fatalf("expected count-mismatch error, got %v", err)
	}
}

// TestCohereCodec_DecodeResponse_embeddings_countMatch_passes is the
// F-0220 positive arm with the request context present.
func TestCohereCodec_DecodeResponse_embeddings_countMatch_passes(t *testing.T) {
	c := newCohereCodec()
	reqBody := []byte(`{"model":"embed-english-v3.0","texts":["a","b"]}`)
	body := []byte(`{"id":"emb","embeddings":[[0.1],[0.2]],"texts":["a","b"]}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json",
		provcore.DecodeContext{RequestBody: reqBody})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got := gjson.GetBytes(decRes.CanonicalBody, "data.#").Int(); got != 2 {
		t.Errorf("data count=%d want 2", got)
	}
}

func TestCohereCodec_DecodeResponse_embeddings_usageExtracted(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{
		"embeddings":[[0.1,0.2]],
		"meta":{"billed_units":{"input_tokens":7}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if gjson.GetBytes(decRes.CanonicalBody, "usage.prompt_tokens").Int() != 7 {
		t.Errorf("prompt_tokens: %s", decRes.CanonicalBody)
	}
	if gjson.GetBytes(decRes.CanonicalBody, "usage.total_tokens").Int() != 7 {
		t.Errorf("total_tokens: %s", decRes.CanonicalBody)
	}
}

func TestCohereCodec_DecodeResponse_embeddings_multiTypeObject_prefersFloat(t *testing.T) {
	// Multi-type embeddings object: prefer float key.
	c := newCohereCodec()
	body := []byte(`{
		"embeddings":{
			"float":[[0.1,0.2],[0.3,0.4]],
			"int8":[[10,20],[30,40]]
		},
		"meta":{"billed_units":{"input_tokens":5}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	data := gjson.GetBytes(decRes.CanonicalBody, "data")
	if !data.IsArray() || len(data.Array()) != 2 {
		t.Fatalf("data must be 2-element array: %s", decRes.CanonicalBody)
	}
	// Should use float values (0.1) not int8 values (10).
	emb0 := data.Array()[0].Get("embedding.0")
	if emb0.Float() != 0.1 {
		t.Errorf("float embedding should be preferred: got %f, want 0.1 in %s", emb0.Float(), decRes.CanonicalBody)
	}
}

func TestCohereCodec_DecodeResponse_embeddings_multiTypeObject_firstKeyFallback(t *testing.T) {
	// Multi-type object without "float" key → use first key.
	c := newCohereCodec()
	body := []byte(`{
		"embeddings":{
			"int8":[[10,20],[30,40]]
		},
		"meta":{"billed_units":{"input_tokens":3}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	data := gjson.GetBytes(decRes.CanonicalBody, "data")
	if !data.IsArray() || len(data.Array()) != 2 {
		t.Fatalf("data must be 2-element array: %s", decRes.CanonicalBody)
	}
}

func TestCohereCodec_DecodeResponse_embeddings_emptyBody_passthrough(t *testing.T) {
	c := newCohereCodec()
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, []byte{}, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(decRes.CanonicalBody) != 0 {
		t.Errorf("expected empty output: %s", decRes.CanonicalBody)
	}
}

func TestCohereCodec_DecodeResponse_embeddings_indexPreserved(t *testing.T) {
	// Index field on each data item should reflect position.
	c := newCohereCodec()
	body := []byte(`{"embeddings":[[0.1],[0.2],[0.3]],"meta":{"billed_units":{"input_tokens":3}}}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	data := gjson.GetBytes(decRes.CanonicalBody, "data").Array()
	for i, item := range data {
		if item.Get("index").Int() != int64(i) {
			t.Errorf("data[%d].index: got %d, want %d", i, item.Get("index").Int(), i)
		}
	}
}

// Fix #9: returned_embedding_type metadata stamping

// TestCohereEmbed_MultiType_StampsReturnedType verifies that when Cohere returns
// a multi-type embeddings object (float + int8 keys), the canonical response
// carries nexus.ext.cohere.returned_embedding_type="float" (the preferred type).
// The field is audit/metadata only and must NOT appear in the data[] or usage
// objects of the canonical body.
func TestCohereEmbed_MultiType_StampsReturnedType(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{
		"embeddings":{
			"float":[[0.1,0.2],[0.3,0.4]],
			"int8":[[10,20],[30,40]]
		},
		"meta":{"billed_units":{"input_tokens":5}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	typ := gjson.GetBytes(decRes.CanonicalBody, "nexus.ext.cohere.returned_embedding_type")
	if !typ.Exists() {
		t.Fatalf("nexus.ext.cohere.returned_embedding_type missing from canonical: %s", decRes.CanonicalBody)
	}
	if typ.Str != "float" {
		t.Errorf("returned_embedding_type = %q, want float", typ.Str)
	}
	// Verify it does NOT pollute the data[].embedding entries (data[] must still
	// carry float vectors from the float key, not the metadata string).
	data := gjson.GetBytes(decRes.CanonicalBody, "data")
	if !data.IsArray() || len(data.Array()) != 2 {
		t.Fatalf("data must be 2-element array: %s", decRes.CanonicalBody)
	}
	emb0 := data.Array()[0].Get("embedding.0")
	if emb0.Float() != 0.1 {
		t.Errorf("float embedding should be preferred: got %f, want 0.1", emb0.Float())
	}
}

// TestCohereEmbed_MultiType_FirstKeyFallback_StampsReturnedType verifies that
// when the multi-type object has no "float" key, the first available key is
// selected and its name is stamped as returned_embedding_type.
func TestCohereEmbed_MultiType_FirstKeyFallback_StampsReturnedType(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{
		"embeddings":{
			"int8":[[10,20],[30,40]]
		},
		"meta":{"billed_units":{"input_tokens":3}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	typ := gjson.GetBytes(decRes.CanonicalBody, "nexus.ext.cohere.returned_embedding_type")
	if !typ.Exists() {
		t.Fatalf("nexus.ext.cohere.returned_embedding_type missing from canonical: %s", decRes.CanonicalBody)
	}
	if typ.Str != "int8" {
		t.Errorf("returned_embedding_type = %q, want int8", typ.Str)
	}
}

// TestCohereEmbed_FlatArray_NoReturnedTypeField verifies that a flat embeddings
// array (Case 1 — single type) does NOT stamp returned_embedding_type because
// the type is not ambiguous in the flat-array shape.
func TestCohereEmbed_FlatArray_NoReturnedTypeField(t *testing.T) {
	c := newCohereCodec()
	body := []byte(`{
		"embeddings":[[0.1,0.2],[0.3,0.4]],
		"meta":{"billed_units":{"input_tokens":4}}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeCohereEmbed, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	typ := gjson.GetBytes(decRes.CanonicalBody, "nexus.ext.cohere.returned_embedding_type")
	if typ.Exists() {
		t.Errorf("returned_embedding_type must NOT be present for flat-array responses, got %q", typ.Str)
	}
}

func TestCohereSpec_RequestShapesContainsEmbeddings(t *testing.T) {
	spec := cohere.NewSpec(nil)
	has := false
	for _, s := range spec.RequestShapes {
		if s == typology.WireShapeCohereEmbed {
			has = true
		}
	}
	if !has {
		t.Errorf("RequestShapes must contain 'embeddings', got %v", spec.RequestShapes)
	}
}
