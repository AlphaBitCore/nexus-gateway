package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// TestParseEmbeddingRequest_SingleString verifies a single string input produces BatchSize=1.
func TestParseEmbeddingRequest_SingleString(t *testing.T) {
	body := []byte(`{"model":"text-embed","input":"hello world"}`)
	req := parseEmbeddingRequest(body)
	if req.BatchSize != 1 {
		t.Errorf("BatchSize = %d, want 1", req.BatchSize)
	}
	if req.Dimensions != nil {
		t.Errorf("Dimensions should be nil when not present in body")
	}
	if req.EncodingFormat != "" {
		t.Errorf("EncodingFormat should be empty")
	}
}

// TestParseEmbeddingRequest_ArrayInput verifies array input produces correct BatchSize.
func TestParseEmbeddingRequest_ArrayInput(t *testing.T) {
	body := []byte(`{"model":"text-embed","input":["hello","world","foo"]}`)
	req := parseEmbeddingRequest(body)
	if req.BatchSize != 3 {
		t.Errorf("BatchSize = %d, want 3", req.BatchSize)
	}
}

// TestParseEmbeddingRequest_EmptyArray produces BatchSize=1 (safe minimum).
func TestParseEmbeddingRequest_EmptyArray(t *testing.T) {
	body := []byte(`{"model":"text-embed","input":[]}`)
	req := parseEmbeddingRequest(body)
	if req.BatchSize != 1 {
		t.Errorf("BatchSize for empty array = %d, want 1", req.BatchSize)
	}
}

// TestParseEmbeddingRequest_Dimensions verifies dimensions extraction.
func TestParseEmbeddingRequest_Dimensions(t *testing.T) {
	body := []byte(`{"model":"text-embed","input":"hello","dimensions":1536}`)
	req := parseEmbeddingRequest(body)
	if req.Dimensions == nil {
		t.Fatal("Dimensions should be non-nil")
	}
	if *req.Dimensions != 1536 {
		t.Errorf("Dimensions = %d, want 1536", *req.Dimensions)
	}
}

// TestParseEmbeddingRequest_EncodingFormat verifies encoding_format extraction.
func TestParseEmbeddingRequest_EncodingFormat(t *testing.T) {
	body := []byte(`{"model":"text-embed","input":"hello","encoding_format":"base64"}`)
	req := parseEmbeddingRequest(body)
	if req.EncodingFormat != "base64" {
		t.Errorf("EncodingFormat = %q, want %q", req.EncodingFormat, "base64")
	}
}

// TestParseEmbeddingRequest_CohereInputType verifies nexus.ext.cohere.input_type extraction.
func TestParseEmbeddingRequest_CohereInputType(t *testing.T) {
	body := []byte(`{"model":"embed-v3","input":"hello","nexus":{"ext":{"cohere":{"input_type":"search_query"}}}}`)
	req := parseEmbeddingRequest(body)
	if req.InputType != "search_query" {
		t.Errorf("InputType = %q, want %q", req.InputType, "search_query")
	}
}

// TestParseEmbeddingRequest_GeminiTaskType verifies nexus.ext.gemini.taskType extraction.
func TestParseEmbeddingRequest_GeminiTaskType(t *testing.T) {
	body := []byte(`{"model":"gemini-embed","input":"hello","nexus":{"ext":{"gemini":{"taskType":"RETRIEVAL_QUERY"}}}}`)
	req := parseEmbeddingRequest(body)
	if req.TaskType != "RETRIEVAL_QUERY" {
		t.Errorf("TaskType = %q, want %q", req.TaskType, "RETRIEVAL_QUERY")
	}
}

// TestParseEmbeddingRequest_EmptyBody produces a default request with BatchSize=1.
func TestParseEmbeddingRequest_EmptyBody(t *testing.T) {
	req := parseEmbeddingRequest(nil)
	if req.BatchSize != 1 {
		t.Errorf("BatchSize for nil body = %d, want 1", req.BatchSize)
	}
}

// TestParseEmbeddingRequest_EmptyByteSlice tests empty non-nil slice.
func TestParseEmbeddingRequest_EmptyByteSlice(t *testing.T) {
	req := parseEmbeddingRequest([]byte{})
	if req.BatchSize != 1 {
		t.Errorf("BatchSize for empty slice = %d, want 1", req.BatchSize)
	}
}

// TestWriteNoCompatibleCapability verifies the 400 envelope shape.
func TestWriteNoCompatibleCapability(t *testing.T) {
	h := &Handler{deps: &Deps{Logger: slog.Default(), AuditWriter: audit.NewWriter(nil, "test", nil, slog.Default())}}
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{}

	e := &routingcore.NoCompatibleProviderError{
		Available: []routingcore.CandidateCapability{
			{
				Provider:            "openai",
				Model:               "ada-002",
				SupportedDimensions: []int{1536},
				MaxBatchSize:        2048,
			},
		},
	}

	h.writeNoCompatibleCapability(rec, auditRec, e)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if auditRec.StatusCode != http.StatusBadRequest {
		t.Errorf("auditRec.StatusCode = %d, want 400", auditRec.StatusCode)
	}
	if auditRec.HookReasonCode != "no_compatible_capability" {
		t.Errorf("HookReasonCode = %q, want %q", auditRec.HookReasonCode, "no_compatible_capability")
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field missing or wrong type: %v", payload)
	}
	if errObj["type"] != "no_compatible_capability" {
		t.Errorf("error.type = %v, want no_compatible_capability", errObj["type"])
	}
	if errObj["code"] != "no_compatible_capability" {
		t.Errorf("error.code = %v, want no_compatible_capability", errObj["code"])
	}
	caps, ok := errObj["available_capabilities"].([]any)
	if !ok {
		t.Fatalf("available_capabilities missing or wrong type: %v", errObj)
	}
	if len(caps) != 1 {
		t.Errorf("available_capabilities len = %d, want 1", len(caps))
	}
}

// TestWriteNoCompatibleCapability_Empty verifies the 400 envelope works with
// an empty Available slice.
func TestWriteNoCompatibleCapability_Empty(t *testing.T) {
	h := &Handler{deps: &Deps{Logger: slog.Default(), AuditWriter: audit.NewWriter(nil, "test", nil, slog.Default())}}
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{}

	e := &routingcore.NoCompatibleProviderError{Available: nil}
	h.writeNoCompatibleCapability(rec, auditRec, e)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj := payload["error"].(map[string]any)
	if errObj["type"] != "no_compatible_capability" {
		t.Errorf("error.type = %v, want no_compatible_capability", errObj["type"])
	}
}
