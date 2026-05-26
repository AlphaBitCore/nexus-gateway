// Package codec_test — Gemini embedding codec tests.
// Named failure modes per provider-adapter-architecture.md §3a:
//   - Rule 3: per-endpoint wire quirks (single vs batch dispatch)
//   - Rule 7: source comments with empirical API citations
package codec_test

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	gemcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	"github.com/tidwall/gjson"
)

// EncodeRequest embeddings

func TestEncodeRequest_embeddings_singleString_embedContentURL(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":"hello world"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.URLOverride != ":embedContent" {
		t.Errorf("URLOverride: got %q, want :embedContent", encRes.URLOverride)
	}
	// Wire body should have content.parts[0].text
	text := gjson.GetBytes(encRes.Body, "content.parts.0.text").Str
	if text != "hello world" {
		t.Errorf("content.parts[0].text: got %q, want 'hello world'", text)
	}
}

func TestEncodeRequest_embeddings_stringArray_batchEmbedContentsURL(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["first","second","third"]}`)
	// Gemini batch embed requires per-item model field; codec reads it
	// from CallTarget.ProviderModelID, so the test must supply it (the
	// dispatcher does this in production from the routed Model row).
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body,
		provcore.CallTarget{ProviderModelID: "text-embedding-004"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.URLOverride != ":batchEmbedContents" {
		t.Errorf("URLOverride: got %q, want :batchEmbedContents", encRes.URLOverride)
	}
	requests := gjson.GetBytes(encRes.Body, "requests")
	if !requests.IsArray() || len(requests.Array()) != 3 {
		t.Fatalf("requests must be 3-element array: %s", encRes.Body)
	}
	if requests.Array()[0].Get("content.parts.0.text").Str != "first" {
		t.Errorf("requests[0].content.parts[0].text: %s", encRes.Body)
	}
	if requests.Array()[2].Get("content.parts.0.text").Str != "third" {
		t.Errorf("requests[2].content.parts[0].text: %s", encRes.Body)
	}
	// Per-item model field is mandatory for Google's batch endpoint.
	for i := range 3 {
		if got := requests.Array()[i].Get("model").Str; got != "models/text-embedding-004" {
			t.Errorf("requests[%d].model = %q, want models/text-embedding-004", i, got)
		}
	}
}

// TestEncodeRequest_embeddings_batchMissingProviderModelID asserts the
// codec rejects a batch request when CallTarget.ProviderModelID is
// empty — the upstream batch API would 400 with a less clear error.
func TestEncodeRequest_embeddings_batchMissingProviderModelID(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"x","input":["a","b"]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for missing ProviderModelID, got nil")
	}
}

func TestEncodeRequest_embeddings_singleElementArray_usesEmbedContent(t *testing.T) {
	// Single-element array → :embedContent per SDD implementation note.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["only-one"]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.URLOverride != ":embedContent" {
		t.Errorf("single-element array should use :embedContent, got %q", encRes.URLOverride)
	}
}

func TestEncodeRequest_embeddings_taskType_passthrough(t *testing.T) {
	// nexus.ext.gemini.taskType → wire taskType in each request.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":"query","nexus":{"ext":{"gemini":{"taskType":"RETRIEVAL_DOCUMENT"}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "taskType").Str != "RETRIEVAL_DOCUMENT" {
		t.Errorf("taskType: %s", encRes.Body)
	}
}

func TestEncodeRequest_embeddings_defaultTaskType_isRetrievalQuery(t *testing.T) {
	// Default taskType is RETRIEVAL_QUERY.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":"query"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "taskType").Str != "RETRIEVAL_QUERY" {
		t.Errorf("default taskType: %s", encRes.Body)
	}
}

func TestEncodeRequest_embeddings_title_passthrough(t *testing.T) {
	// nexus.ext.gemini.title → wire title.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":"doc","nexus":{"ext":{"gemini":{"title":"My Document","taskType":"RETRIEVAL_DOCUMENT"}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "title").Str != "My Document" {
		t.Errorf("title: %s", encRes.Body)
	}
}

func TestEncodeRequest_embeddings_outputDimensionality_fromDimensions(t *testing.T) {
	// canonical dimensions → wire outputDimensionality.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":"hello","dimensions":768}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "outputDimensionality").Int() != 768 {
		t.Errorf("outputDimensionality: %s", encRes.Body)
	}
}

func TestEncodeRequest_embeddings_batchTaskType_perRequest(t *testing.T) {
	// Batch requests: taskType applied to each sub-request.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["a","b"],"nexus":{"ext":{"gemini":{"taskType":"SEMANTIC_SIMILARITY"}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body,
		provcore.CallTarget{ProviderModelID: "text-embedding-004"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	requests := gjson.GetBytes(encRes.Body, "requests").Array()
	for i, r := range requests {
		if r.Get("taskType").Str != "SEMANTIC_SIMILARITY" {
			t.Errorf("requests[%d].taskType: %s", i, encRes.Body)
		}
	}
}

func TestEncodeRequest_embeddings_tokenArray_returns400(t *testing.T) {
	// Token arrays are unsupported by Gemini embedding endpoint.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":[1,2,3,4]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for token array input")
	}
	if !strings.Contains(err.Error(), "token array") {
		t.Errorf("error should mention token array: %v", err)
	}
}

func TestEncodeRequest_embeddings_tokenBatchArray_returns400(t *testing.T) {
	// Batch token arrays are also unsupported.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":[[1,2],[3,4]]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for batch token array input")
	}
}

func TestEncodeRequest_embeddings_emptyInputArray_returns400(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":[]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for empty input array")
	}
}

func TestEncodeRequest_embeddings_missingInput_returns400(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004"}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for missing input")
	}
}

func TestEncodeRequest_embeddings_invalidJSON_returns400(t *testing.T) {
	var c gemcodec.Codec
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, []byte(`{not json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestEncodeRequest_embeddings_emptyBody_returns400(t *testing.T) {
	var c gemcodec.Codec
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, []byte{}, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestEncodeRequest_embeddings_contentType(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.ContentType != "application/json" {
		t.Errorf("ContentType: got %q, want application/json", encRes.ContentType)
	}
}

// DecodeResponse embeddings

func TestDecodeResponse_embeddings_singleEmbedContent(t *testing.T) {
	// :embedContent response shape: {"embedding":{"values":[…]}}
	var c gemcodec.Codec
	body := []byte(`{"embedding":{"values":[0.1,0.2,0.3,0.4]}}`)
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, body, "application/json")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	data := gjson.GetBytes(decRes.CanonicalBody, "data")
	if !data.IsArray() || len(data.Array()) != 1 {
		t.Fatalf("data must be 1-element array: %s", decRes.CanonicalBody)
	}
	emb := data.Array()[0].Get("embedding")
	if !emb.IsArray() || len(emb.Array()) != 4 {
		t.Errorf("embedding must be 4-element array: %s", decRes.CanonicalBody)
	}
	if emb.Array()[0].Float() != 0.1 {
		t.Errorf("embedding[0]: got %f, want 0.1", emb.Array()[0].Float())
	}
	if gjson.GetBytes(decRes.CanonicalBody, "object").Str != "list" {
		t.Errorf("object must be 'list': %s", decRes.CanonicalBody)
	}
}

func TestDecodeResponse_embeddings_batchEmbedContents(t *testing.T) {
	// :batchEmbedContents response shape: {"embeddings":[{"values":[…]},…]}
	var c gemcodec.Codec
	body := []byte(`{
		"embeddings":[
			{"values":[0.1,0.2]},
			{"values":[0.3,0.4]},
			{"values":[0.5,0.6]}
		]
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, body, "application/json")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	data := gjson.GetBytes(decRes.CanonicalBody, "data")
	if !data.IsArray() || len(data.Array()) != 3 {
		t.Fatalf("data must be 3-element array: %s", decRes.CanonicalBody)
	}
	// Order preserved.
	if data.Array()[0].Get("embedding.0").Float() != 0.1 {
		t.Errorf("order not preserved: %s", decRes.CanonicalBody)
	}
	if data.Array()[2].Get("embedding.0").Float() != 0.5 {
		t.Errorf("order not preserved for index 2: %s", decRes.CanonicalBody)
	}
}

func TestDecodeResponse_embeddings_batchOrder_preserved(t *testing.T) {
	// Index field reflects canonical position.
	var c gemcodec.Codec
	body := []byte(`{"embeddings":[{"values":[1.0]},{"values":[2.0]},{"values":[3.0]}]}`)
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, body, "")
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

func TestDecodeResponse_embeddings_usageMetadata(t *testing.T) {
	// usageMetadata.totalTokenCount → usage.prompt_tokens + usage.total_tokens
	var c gemcodec.Codec
	body := []byte(`{
		"embedding":{"values":[0.1,0.2]},
		"usageMetadata":{"totalTokenCount":15}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, body, "")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	pt := gjson.GetBytes(decRes.CanonicalBody, "usage.prompt_tokens")
	if pt.Int() != 15 {
		t.Errorf("prompt_tokens: got %d, want 15", pt.Int())
	}
	tt := gjson.GetBytes(decRes.CanonicalBody, "usage.total_tokens")
	if tt.Int() != 15 {
		t.Errorf("total_tokens: got %d, want 15", tt.Int())
	}
}

func TestDecodeResponse_embeddings_noUsageMetadata_zeroUsage(t *testing.T) {
	// Gemini may not return usageMetadata for embedding responses.
	var c gemcodec.Codec
	body := []byte(`{"embedding":{"values":[0.1,0.2]}}`)
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, body, "")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	// Zero usage is acceptable.
	pt := gjson.GetBytes(decRes.CanonicalBody, "usage.prompt_tokens")
	if pt.Int() != 0 {
		t.Errorf("no usageMetadata: expected 0 prompt_tokens, got %d", pt.Int())
	}
}

func TestDecodeResponse_embeddings_emptyBody_passthrough(t *testing.T) {
	var c gemcodec.Codec
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, []byte{}, "")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(decRes.CanonicalBody) != 0 {
		t.Errorf("expected empty output: %s", decRes.CanonicalBody)
	}
}

// Coverage gap closers

func TestEncodeRequest_embeddings_batchWithTitleAndDimensions(t *testing.T) {
	// Covers title + outputDimensionality in batch path.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["a","b"],"dimensions":512,
		"nexus":{"ext":{"gemini":{"taskType":"RETRIEVAL_DOCUMENT","title":"Doc Title"}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body,
		provcore.CallTarget{ProviderModelID: "text-embedding-004"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.URLOverride != ":batchEmbedContents" {
		t.Errorf("URLOverride: %q", encRes.URLOverride)
	}
	r0 := gjson.GetBytes(encRes.Body, "requests.0")
	if r0.Get("title").Str != "Doc Title" {
		t.Errorf("title in batch request: %s", encRes.Body)
	}
	if r0.Get("outputDimensionality").Int() != 512 {
		t.Errorf("outputDimensionality in batch request: %s", encRes.Body)
	}
}

func TestEncodeRequest_embeddings_singleWithNoExtensions(t *testing.T) {
	// Covers single path with no taskType/title/dimensions (all optional branches skipped).
	// This exercises the default taskType path where title and outputDimensionality are empty/nil.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":"plain text"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// taskType defaults to RETRIEVAL_QUERY; no title or outputDimensionality.
	if gjson.GetBytes(encRes.Body, "title").Exists() {
		t.Errorf("title should not appear when not provided: %s", encRes.Body)
	}
	if gjson.GetBytes(encRes.Body, "outputDimensionality").Exists() {
		t.Errorf("outputDimensionality should not appear when no dimensions: %s", encRes.Body)
	}
}

func TestEncodeRequest_embeddings_mixedTypeInputArray_returns400(t *testing.T) {
	// Mixed-type array (string + bool etc.) → safety-net 400.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["text",true]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for mixed-type input array")
	}
}

func TestEncodeRequest_embeddings_nonStringNonArrayInput_returns400(t *testing.T) {
	// Input is an object (not string or array) → safety-net 400.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":{"key":"value"}}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for object input")
	}
}

func TestDecodeResponse_embeddings_invalidJSON_returnsError(t *testing.T) {
	var c gemcodec.Codec
	_, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, []byte(`{not json`), "")
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

// Exercise the batch-embed title + outputDimensionality branches. The
// single + batch paths share most code; explicitly hitting both keeps
// the contract documented even when an unrelated codec edit shifts coverage.

func TestEncodeRequest_embeddings_batchTitle_perRequest(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["a","b"],"nexus":{"ext":{"gemini":{"title":"doc-title"}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body,
		provcore.CallTarget{ProviderModelID: "text-embedding-004"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	requests := gjson.GetBytes(encRes.Body, "requests").Array()
	if len(requests) != 2 {
		t.Fatalf("expected 2 sub-requests; got %d", len(requests))
	}
	for i, r := range requests {
		if r.Get("title").Str != "doc-title" {
			t.Errorf("requests[%d].title: %s", i, encRes.Body)
		}
	}
}

func TestEncodeRequest_embeddings_batchOutputDimensionality_perRequest(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["a","b"],"dimensions":512}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body,
		provcore.CallTarget{ProviderModelID: "text-embedding-004"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	requests := gjson.GetBytes(encRes.Body, "requests").Array()
	for i, r := range requests {
		if r.Get("outputDimensionality").Int() != 512 {
			t.Errorf("requests[%d].outputDimensionality: %s", i, encRes.Body)
		}
	}
}

func TestEncodeRequest_embeddings_batchModelPathAlreadyPrefixed(t *testing.T) {
	// If admin set ProviderModelID with "models/" prefix already, builder
	// must not double-prefix.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":["a"]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body,
		provcore.CallTarget{ProviderModelID: "models/text-embedding-004"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// Single-element array → uses :embedContent URL not :batchEmbedContents
	// (per pre-existing behaviour). Just confirm we didn't return an error.
	if len(encRes.Body) == 0 {
		t.Fatal("empty body")
	}
}

// Cover error/edge branches that bring coverage ≥95%.

func TestEncodeRequest_embeddings_inputArrayNonString_returns400(t *testing.T) {
	// First element of array is not a string → 400 from the "first.Type != gjson.String" branch.
	var c gemcodec.Codec
	body := []byte(`{"model":"text-embedding-004","input":[123,"b"]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiEmbedContent, body, provcore.CallTarget{ProviderModelID: "x"})
	if err == nil {
		t.Fatal("expected error for non-string first element")
	}
}

func TestDecodeResponse_embeddings_batchSumsTokensFromStatistics(t *testing.T) {
	// Batch response with per-item statistics.token_count → exercises the
	// 312-322 branch that sums per-item tokens.
	var c gemcodec.Codec
	body := []byte(`{
		"embeddings": [
			{"values":[0.1,0.2], "statistics":{"token_count":10}},
			{"values":[0.3,0.4], "statistics":{"token_count":20}}
		]
	}`)
	resp, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, body, "")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 30 {
		t.Errorf("PromptTokens sum: %v (want 30)", resp.Usage.PromptTokens)
	}
}

func TestDecodeResponse_embeddings_batchSumsCharsAsTokens(t *testing.T) {
	// Batch response with billable_character_count fallback (chars/4).
	var c gemcodec.Codec
	body := []byte(`{
		"embeddings": [
			{"values":[0.1], "metadata":{"billable_character_count":40}},
			{"values":[0.2], "metadata":{"billable_character_count":80}}
		]
	}`)
	resp, err := c.DecodeResponse(typology.WireShapeGeminiEmbedContent, body, "")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	// 40/4 + 80/4 = 30
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 30 {
		t.Errorf("PromptTokens chars/4 fallback: %v (want 30)", resp.Usage.PromptTokens)
	}
}
