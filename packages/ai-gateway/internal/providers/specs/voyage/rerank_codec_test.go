// Package voyage_test — rerank codec tests for the Voyage AI SchemaCodec.
//
// The canonical rerank shape is Cohere's, so this adapter owns the
// canonical→Voyage translation and back. These tests exercise the
// WireShapeVoyageRerank dispatch in codec.go plus the encodeVoyageRerank /
// decodeVoyageRerankResponse legs:
//   - encode: the canonical `top_n` → Voyage wire `top_k` rename, document +
//     return_documents preservation, and every named 400 failure mode.
//   - decode: Voyage `data[]` → canonical `results[]` with the bare document
//     string wrapped as {text:...}, meta.billed_units.total_tokens surfaced for
//     the client contract, AND — the C1 regression guard — usage.total_tokens
//     stamped as DecodeResult.Usage.PromptTokens so BillableUnits prices it
//     (stamping TotalTokens only would price rerank at $0).
package voyage_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/voyage"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func newVoyageCodec() provcore.SchemaCodec {
	return voyage.NewSpec(nil).SchemaCodec
}

// assertProviderError400 fails unless err is a *provcore.ProviderError with a
// 400 status whose message contains want.
func assertProviderError400(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a 400 ProviderError containing %q, got nil", want)
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", pe.Status)
	}
	if !strings.Contains(pe.Message, want) {
		t.Errorf("message = %q, want substring %q", pe.Message, want)
	}
}

// EncodeRequest rerank

func TestVoyageCodec_EncodeRequest_rerank_invalidBody_returns400(t *testing.T) {
	c := newVoyageCodec()
	_, err := c.EncodeRequest(typology.WireShapeVoyageRerank, []byte(`{not json`), provcore.CallTarget{})
	assertProviderError400(t, err, "invalid canonical rerank body")
}

func TestVoyageCodec_EncodeRequest_rerank_missingModel_returns400(t *testing.T) {
	// No target model and no body model → 400.
	c := newVoyageCodec()
	body := []byte(`{"query":"q","documents":["a","b"]}`)
	_, err := c.EncodeRequest(typology.WireShapeVoyageRerank, body, provcore.CallTarget{})
	assertProviderError400(t, err, "missing model")
}

func TestVoyageCodec_EncodeRequest_rerank_emptyQuery_returns400(t *testing.T) {
	c := newVoyageCodec()
	body := []byte(`{"model":"rerank-2","query":"","documents":["a"]}`)
	_, err := c.EncodeRequest(typology.WireShapeVoyageRerank, body, provcore.CallTarget{})
	assertProviderError400(t, err, "'query' must be a non-empty string")
}

func TestVoyageCodec_EncodeRequest_rerank_nonStringQuery_returns400(t *testing.T) {
	c := newVoyageCodec()
	body := []byte(`{"model":"rerank-2","query":42,"documents":["a"]}`)
	_, err := c.EncodeRequest(typology.WireShapeVoyageRerank, body, provcore.CallTarget{})
	assertProviderError400(t, err, "'query' must be a non-empty string")
}

func TestVoyageCodec_EncodeRequest_rerank_documentsNotArray_returns400(t *testing.T) {
	c := newVoyageCodec()
	body := []byte(`{"model":"rerank-2","query":"q","documents":"not-an-array"}`)
	_, err := c.EncodeRequest(typology.WireShapeVoyageRerank, body, provcore.CallTarget{})
	assertProviderError400(t, err, "'documents' must be an array")
}

func TestVoyageCodec_EncodeRequest_rerank_nonStringDocument_returns400(t *testing.T) {
	c := newVoyageCodec()
	body := []byte(`{"model":"rerank-2","query":"q","documents":["ok",7]}`)
	_, err := c.EncodeRequest(typology.WireShapeVoyageRerank, body, provcore.CallTarget{})
	assertProviderError400(t, err, "every 'documents' entry must be a string")
}

// TestVoyageCodec_EncodeRequest_rerank_valid_topNRenamedToTopK is the core
// wire-translation assertion: canonical `top_n` becomes Voyage `top_k`, the
// original `top_n` is dropped, and documents + return_documents survive.
func TestVoyageCodec_EncodeRequest_rerank_valid_topNRenamedToTopK(t *testing.T) {
	c := newVoyageCodec()
	body := []byte(`{
		"model":"rerank-2",
		"query":"what is nexus",
		"documents":["doc a","doc b","doc c"],
		"top_n":2,
		"return_documents":true
	}`)
	encRes, err := c.EncodeRequest(typology.WireShapeVoyageRerank, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	wire := encRes.Body
	// top_n → top_k rename: top_k present with the value, top_n gone.
	if got := gjson.GetBytes(wire, "top_k"); !got.Exists() || got.Int() != 2 {
		t.Errorf("top_k = %v, want 2; wire=%s", got.Raw, wire)
	}
	if gjson.GetBytes(wire, "top_n").Exists() {
		t.Errorf("top_n must NOT appear in Voyage wire (renamed to top_k): %s", wire)
	}
	// documents preserved verbatim.
	docs := gjson.GetBytes(wire, "documents")
	if !docs.IsArray() || len(docs.Array()) != 3 {
		t.Fatalf("documents must be a 3-element array: %s", wire)
	}
	arr := docs.Array()
	if arr[0].Str != "doc a" || arr[1].Str != "doc b" || arr[2].Str != "doc c" {
		t.Errorf("documents content changed: %s", wire)
	}
	// return_documents preserved.
	if rd := gjson.GetBytes(wire, "return_documents"); rd.Type != gjson.True {
		t.Errorf("return_documents = %v, want true; wire=%s", rd.Raw, wire)
	}
	// model + query survive.
	if gjson.GetBytes(wire, "model").Str != "rerank-2" {
		t.Errorf("model missing: %s", wire)
	}
	if gjson.GetBytes(wire, "query").Str != "what is nexus" {
		t.Errorf("query missing: %s", wire)
	}
	if encRes.ContentType != "application/json" {
		t.Errorf("ContentType: got %q, want application/json", encRes.ContentType)
	}
}

func TestVoyageCodec_EncodeRequest_rerank_modelFromTarget_returnDocumentsFalse(t *testing.T) {
	// Model resolves from CallTarget; return_documents:false must round-trip as
	// an explicit false (not dropped).
	c := newVoyageCodec()
	body := []byte(`{"query":"q","documents":["a"],"return_documents":false}`)
	encRes, err := c.EncodeRequest(typology.WireShapeVoyageRerank, body, provcore.CallTarget{ProviderModelID: "rerank-2"})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "model").Str != "rerank-2" {
		t.Errorf("model from target missing: %s", encRes.Body)
	}
	if rd := gjson.GetBytes(encRes.Body, "return_documents"); rd.Type != gjson.False {
		t.Errorf("return_documents = %v, want false; wire=%s", rd.Raw, encRes.Body)
	}
	// No top_n supplied → no top_k on the wire.
	if gjson.GetBytes(encRes.Body, "top_k").Exists() {
		t.Errorf("top_k must be absent when top_n omitted: %s", encRes.Body)
	}
}

// DecodeResponse rerank

func TestVoyageCodec_DecodeResponse_rerank_emptyBody_identity(t *testing.T) {
	c := newVoyageCodec()
	decRes, err := c.DecodeResponse(typology.WireShapeVoyageRerank, []byte{}, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(decRes.CanonicalBody) != 0 {
		t.Errorf("empty body must pass through empty: %s", decRes.CanonicalBody)
	}
}

func TestVoyageCodec_DecodeResponse_rerank_invalidJSON_returnsError(t *testing.T) {
	c := newVoyageCodec()
	_, err := c.DecodeResponse(typology.WireShapeVoyageRerank, []byte(`{not json`), "application/json", provcore.DecodeContext{})
	if err == nil {
		t.Fatal("expected error for invalid rerank response JSON")
	}
}

// TestVoyageCodec_DecodeResponse_rerank_valid_wrapsDocument_stampsPromptTokens
// is the C1 $0-cost regression guard. The Voyage response's usage.total_tokens
// MUST land on DecodeResult.Usage.PromptTokens (not only TotalTokens), because
// BillableUnits prices off Prompt/Completion tokens — a TotalTokens-only stamp
// would price Voyage rerank at $0. It also pins the bare-string document →
// {text:...} wrap and the meta.billed_units.total_tokens audit mirror.
func TestVoyageCodec_DecodeResponse_rerank_valid_wrapsDocument_stampsPromptTokens(t *testing.T) {
	c := newVoyageCodec()
	body := []byte(`{
		"object":"list",
		"data":[
			{"index":0,"relevance_score":0.9,"document":"txt"},
			{"index":1,"relevance_score":0.5,"document":"other"}
		],
		"model":"rerank-2",
		"usage":{"total_tokens":26}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeVoyageRerank, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	cb := decRes.CanonicalBody

	// results[] shape: index + relevance_score preserved; bare string document
	// wrapped as {text:...} per the canonical (Cohere) contract.
	results := gjson.GetBytes(cb, "results")
	if !results.IsArray() || len(results.Array()) != 2 {
		t.Fatalf("results must be a 2-element array: %s", cb)
	}
	if got := gjson.GetBytes(cb, "results.0.index").Int(); got != 0 {
		t.Errorf("results[0].index = %d, want 0", got)
	}
	if got := gjson.GetBytes(cb, "results.0.relevance_score").Float(); got != 0.9 {
		t.Errorf("results[0].relevance_score = %v, want 0.9", got)
	}
	if got := gjson.GetBytes(cb, "results.0.document.text").Str; got != "txt" {
		t.Errorf("results[0].document.text = %q, want txt (bare string must be wrapped); body=%s", got, cb)
	}
	if got := gjson.GetBytes(cb, "results.1.document.text").Str; got != "other" {
		t.Errorf("results[1].document.text = %q, want other", got)
	}
	// model surfaced on the canonical body.
	if got := gjson.GetBytes(cb, "model").Str; got != "rerank-2" {
		t.Errorf("model = %q, want rerank-2", got)
	}
	// Audit mirror: meta.billed_units.total_tokens.
	if got := gjson.GetBytes(cb, "meta.billed_units.total_tokens").Int(); got != 26 {
		t.Errorf("meta.billed_units.total_tokens = %d, want 26; body=%s", got, cb)
	}

	// C1 regression guard: PromptTokens MUST be set to 26 so the token cost path
	// prices the request. Asserting only TotalTokens would let the $0 bug back in.
	if decRes.Usage.PromptTokens == nil {
		t.Fatalf("Usage.PromptTokens is nil — Voyage rerank would price at $0 (C1 regression)")
	}
	if *decRes.Usage.PromptTokens != 26 {
		t.Errorf("Usage.PromptTokens = %d, want 26", *decRes.Usage.PromptTokens)
	}
	if decRes.Usage.TotalTokens == nil || *decRes.Usage.TotalTokens != 26 {
		t.Errorf("Usage.TotalTokens = %v, want 26", decRes.Usage.TotalTokens)
	}
}

// TestVoyageCodec_DecodeResponse_rerank_noUsage_leavesUsageEmpty pins that a
// response without usage.total_tokens leaves Usage unset (no phantom 0-token
// pricing) while still producing a valid canonical body.
func TestVoyageCodec_DecodeResponse_rerank_noUsage_leavesUsageEmpty(t *testing.T) {
	c := newVoyageCodec()
	body := []byte(`{"object":"list","data":[{"index":0,"relevance_score":0.7}],"model":"rerank-2"}`)
	decRes, err := c.DecodeResponse(typology.WireShapeVoyageRerank, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if decRes.Usage.PromptTokens != nil || decRes.Usage.TotalTokens != nil {
		t.Errorf("Usage must stay empty when no total_tokens reported, got %+v", decRes.Usage)
	}
	// total_tokens defaults to 0 in the audit mirror.
	if got := gjson.GetBytes(decRes.CanonicalBody, "meta.billed_units.total_tokens").Int(); got != 0 {
		t.Errorf("meta.billed_units.total_tokens = %d, want 0", got)
	}
	// A result with no document field must not synthesize a document key.
	if gjson.GetBytes(decRes.CanonicalBody, "results.0.document").Exists() {
		t.Errorf("document must be absent when Voyage omits it: %s", decRes.CanonicalBody)
	}
}
