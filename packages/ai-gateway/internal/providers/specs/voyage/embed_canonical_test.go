package voyage_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/voyage"
)

func TestEmbedRequestToCanonical_StringInput(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":"hello world"}`)
	canonical, err := voyage.EmbedRequestToCanonical(body, "")
	if err != nil {
		t.Fatalf("EmbedRequestToCanonical: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(canonical, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["model"] != "voyage-3" {
		t.Errorf("model=%v want voyage-3", out["model"])
	}
	if out["input"] != "hello world" {
		t.Errorf("input=%v want hello world", out["input"])
	}
}

func TestEmbedRequestToCanonical_ArrayInput(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":["a","b"]}`)
	canonical, err := voyage.EmbedRequestToCanonical(body, "")
	if err != nil {
		t.Fatalf("EmbedRequestToCanonical: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(canonical, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	arr, ok := out["input"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("input=%v want [a b]", out["input"])
	}
}

func TestEmbedRequestToCanonical_ModelFromProviderModelID(t *testing.T) {
	body := []byte(`{"input":"hello"}`)
	canonical, err := voyage.EmbedRequestToCanonical(body, "voyage-code-3")
	if err != nil {
		t.Fatalf("EmbedRequestToCanonical: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(canonical, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["model"] != "voyage-code-3" {
		t.Errorf("model=%v want voyage-code-3", out["model"])
	}
}

func TestEmbedRequestToCanonical_BodyModelWins(t *testing.T) {
	// When body has model and providerModelID is also set, body model takes
	// precedence (per implementation: body first, fallback to providerModelID).
	body := []byte(`{"model":"voyage-3","input":"hi"}`)
	canonical, err := voyage.EmbedRequestToCanonical(body, "voyage-code-3")
	if err != nil {
		t.Fatalf("EmbedRequestToCanonical: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(canonical, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["model"] != "voyage-3" {
		t.Errorf("model=%v want voyage-3 (body wins over providerModelID)", out["model"])
	}
}

func TestEmbedRequestToCanonical_Extensions(t *testing.T) {
	body := []byte(`{
		"model":"voyage-3",
		"input":"hello",
		"input_type":"query",
		"output_dtype":"float",
		"output_dimension":1024,
		"truncation":true
	}`)
	canonical, err := voyage.EmbedRequestToCanonical(body, "")
	if err != nil {
		t.Fatalf("EmbedRequestToCanonical: %v", err)
	}
	// Extensions land under nexus.ext.voyage.*
	s := string(canonical)
	if !strings.Contains(s, "input_type") {
		t.Errorf("input_type not in canonical: %s", s)
	}
	if !strings.Contains(s, "output_dtype") {
		t.Errorf("output_dtype not in canonical: %s", s)
	}
	if !strings.Contains(s, "output_dimension") {
		t.Errorf("output_dimension not in canonical: %s", s)
	}
	if !strings.Contains(s, "truncation") {
		t.Errorf("truncation not in canonical: %s", s)
	}
}

func TestEmbedRequestToCanonical_InvalidJSON(t *testing.T) {
	_, err := voyage.EmbedRequestToCanonical([]byte(`not-json`), "")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest {
		t.Fatalf("want 400 ProviderError, got %v", err)
	}
}

func TestEmbedRequestToCanonical_MissingInput(t *testing.T) {
	body := []byte(`{"model":"voyage-3"}`)
	_, err := voyage.EmbedRequestToCanonical(body, "")
	if err == nil || !strings.Contains(err.Error(), "missing 'input'") {
		t.Fatalf("expected missing-input error, got %v", err)
	}
}

func TestEmbedRequestToCanonical_MissingModel(t *testing.T) {
	body := []byte(`{"input":"hello"}`)
	_, err := voyage.EmbedRequestToCanonical(body, "")
	if err == nil || !strings.Contains(err.Error(), "missing 'model'") {
		t.Fatalf("expected missing-model error, got %v", err)
	}
}

func TestEmbedRequestToCanonical_NonStringArrayInput(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":[1,2,3]}`)
	_, err := voyage.EmbedRequestToCanonical(body, "")
	if err == nil {
		t.Fatal("non-string array must be rejected")
	}
}

func TestEmbedRequestToCanonical_InputWrongType(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":42}`)
	_, err := voyage.EmbedRequestToCanonical(body, "")
	if err == nil {
		t.Fatal("numeric input must be rejected")
	}
}

func TestCanonicalToEmbedResponse_HappyPath(t *testing.T) {
	canonical := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0}],
		"model":"voyage-3",
		"usage":{"prompt_tokens":5,"total_tokens":5}
	}`)
	out, err := voyage.CanonicalToEmbedResponse(canonical)
	if err != nil {
		t.Fatalf("CanonicalToEmbedResponse: %v", err)
	}
	var parsed map[string]any
	if e := json.Unmarshal(out, &parsed); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if parsed["object"] != "list" {
		t.Errorf("object=%v want list", parsed["object"])
	}
	usage, _ := parsed["usage"].(map[string]any)
	if tt, ok := usage["total_tokens"].(float64); !ok || tt != 5 {
		t.Errorf("usage.total_tokens=%v want 5", usage["total_tokens"])
	}
	// Voyage wire shape does NOT carry prompt_tokens.
	if _, ok := usage["prompt_tokens"]; ok {
		t.Errorf("prompt_tokens must not appear in Voyage wire response")
	}
	data, _ := parsed["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len=%d want 1", len(data))
	}
	item, _ := data[0].(map[string]any)
	if item["object"] != "embedding" {
		t.Errorf("item.object=%v want embedding", item["object"])
	}
	emb, _ := item["embedding"].([]any)
	if len(emb) != 3 {
		t.Errorf("embedding len=%d want 3", len(emb))
	}
}

func TestCanonicalToEmbedResponse_FallsBackToPromptTokens(t *testing.T) {
	// When total_tokens is 0, fall back to prompt_tokens.
	canonical := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1],"index":0}],
		"model":"voyage-3",
		"usage":{"prompt_tokens":7,"total_tokens":0}
	}`)
	out, err := voyage.CanonicalToEmbedResponse(canonical)
	if err != nil {
		t.Fatalf("CanonicalToEmbedResponse: %v", err)
	}
	var parsed map[string]any
	if e := json.Unmarshal(out, &parsed); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	usage, _ := parsed["usage"].(map[string]any)
	if tt, ok := usage["total_tokens"].(float64); !ok || tt != 7 {
		t.Errorf("fallback total_tokens=%v want 7 (from prompt_tokens)", usage["total_tokens"])
	}
}

func TestCanonicalToEmbedResponse_MultipleItems(t *testing.T) {
	canonical := []byte(`{
		"object":"list",
		"data":[
			{"object":"embedding","embedding":[0.1,0.2],"index":0},
			{"object":"embedding","embedding":[0.3,0.4],"index":1}
		],
		"model":"voyage-3",
		"usage":{"prompt_tokens":4,"total_tokens":4}
	}`)
	out, err := voyage.CanonicalToEmbedResponse(canonical)
	if err != nil {
		t.Fatalf("CanonicalToEmbedResponse: %v", err)
	}
	var parsed map[string]any
	if e := json.Unmarshal(out, &parsed); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	data, _ := parsed["data"].([]any)
	if len(data) != 2 {
		t.Errorf("data len=%d want 2", len(data))
	}
}

func TestCanonicalToEmbedResponse_InvalidJSON(t *testing.T) {
	_, err := voyage.CanonicalToEmbedResponse([]byte(`not-json`))
	if err == nil || !strings.Contains(err.Error(), "invalid canonical body") {
		t.Fatalf("expected invalid-canonical error, got %v", err)
	}
}

func TestCanonicalToEmbedResponse_MissingData(t *testing.T) {
	_, err := voyage.CanonicalToEmbedResponse([]byte(`{"object":"list","model":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "missing data") {
		t.Fatalf("expected missing-data error, got %v", err)
	}
}

func TestCanonicalToEmbedResponse_MissingEmbeddingInItem(t *testing.T) {
	canonical := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","index":0}],
		"model":"voyage-3",
		"usage":{"total_tokens":1}
	}`)
	_, err := voyage.CanonicalToEmbedResponse(canonical)
	if err == nil || !strings.Contains(err.Error(), "embedding missing") {
		t.Fatalf("expected missing-embedding error, got %v", err)
	}
}
