package azure

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "azure-openai" {
		t.Errorf("ID = %q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil) = %v", err)
	}
}

func TestRemapPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "chat completions",
			input:    "/openai/deployments/gpt-4/chat/completions?api-version=2024-02-01",
			expected: "/v1/chat/completions",
		},
		{
			name:     "embeddings",
			input:    "/openai/deployments/text-embedding-ada-002/embeddings?api-version=2024-02-01",
			expected: "/v1/embeddings",
		},
		{
			name:     "no query string",
			input:    "/openai/deployments/my-model/chat/completions",
			expected: "/v1/chat/completions",
		},
		{
			name:     "non-azure path passthrough",
			input:    "/v1/chat/completions",
			expected: "/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remapPath(tt.input)
			if got != tt.expected {
				t.Errorf("remapPath(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestExtractRequest_AzureChatCompletions(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello!"}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/openai/deployments/gpt-4/chat/completions?api-version=2024-02-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "You are helpful." {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
	if nc.Segments[1] != "Hello!" {
		t.Errorf("segment[1] = %q", nc.Segments[1])
	}
}

func TestExtractRequest_AzureEmbeddings(t *testing.T) {
	body := []byte(`{
		"model": "text-embedding-ada-002",
		"input": "Hello, world!"
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/openai/deployments/text-embedding-ada-002/embeddings?api-version=2024-02-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello, world!" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/openai/deployments/gpt-4/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractRequest_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"gpt-4"}`), "/openai/deployments/gpt-4/chat/completions")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractResponse_AzureChatCompletions(t *testing.T) {
	body := []byte(`{
		"choices": [
			{"message": {"role": "assistant", "content": "Hello! How can I help?"}}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/openai/deployments/gpt-4/chat/completions?api-version=2024-02-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello! How can I help?" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractStreamChunk(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/openai/deployments/gpt-4/chat/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractStreamChunk_EmptyDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{}}]}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/openai/deployments/gpt-4/chat/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("expected 0 segments for empty delta, got %d", len(nc.Segments))
	}
}

// TestExtractRequest_AzureToolCallsDelegation pins that the Azure
// adapter, which delegates to openai-compat after path remapping,
// inherits the tool_calls extraction — Azure
// OpenAI carries the same wire format, so assistant tool_calls in
// conversation history must surface on ToolCallSegments through the
// delegation chain.
func TestExtractRequest_AzureToolCallsDelegation(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":"weather in NYC?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/openai/deployments/gpt-4o/chat/completions?api-version=2024-08-01")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v want delegation to surface tool_calls", nc.ToolCallSegments)
	}
}

// TestExtractResponse_AzureToolCallsDelegation pins response-side
// delegation: assistant message tool_calls and finish_reason flow
// through Azure → openai-compat correctly.
func TestExtractResponse_AzureToolCallsDelegation(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-abc","model":"gpt-4o",
		"system_fingerprint":"fp_xyz",
		"choices":[{
			"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_b","type":"function","function":{"name":"send_email","arguments":"{}"}}
			]},
			"finish_reason":"tool_calls"
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/openai/deployments/gpt-4o/chat/completions?api-version=2024-08-01")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	if nc.Metadata["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
	if nc.Metadata["system_fingerprint"] != "fp_xyz" {
		t.Errorf("system_fingerprint=%q", nc.Metadata["system_fingerprint"])
	}
}

func TestExtractStreamChunk_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/openai/deployments/gpt-4/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}
