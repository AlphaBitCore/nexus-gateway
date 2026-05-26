package minimax

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "minimax" {
		t.Errorf("ID = %q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil) = %v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map) = %v", err)
	}
}

func TestExtractRequest_NativeFormat(t *testing.T) {
	body := []byte(`{
		"model": "abab5.5-chat",
		"prompt": "You are a helpful assistant.",
		"messages": [
			{"sender_type": "USER", "text": "Hello MiniMax!"},
			{"sender_type": "BOT", "text": "Hi there!"}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/text/chatcompletion")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 3 {
		t.Fatalf("expected 3 segments (prompt + 2 messages), got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "You are a helpful assistant." {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
	if nc.Segments[1] != "Hello MiniMax!" {
		t.Errorf("segment[1] = %q", nc.Segments[1])
	}
	if nc.Metadata["model"] != "abab5.5-chat" {
		t.Errorf("model = %q", nc.Metadata["model"])
	}
}

func TestExtractRequest_CompatFormat(t *testing.T) {
	// MiniMax's OpenAI-compatible /chat/completions on api.minimax.io is
	// the primary surface for the M2 family. The adapter still detects
	// native abab*-style bodies (TestExtractRequest_NativeFormat) for
	// agent-intercepted legacy traffic; the compat path is what new
	// deployments target.
	body := []byte(`{
		"model": "MiniMax-M2.7",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello!"}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
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
	if nc.Metadata["model"] != "MiniMax-M2.7" {
		t.Errorf("model = %q", nc.Metadata["model"])
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/v1/text/chatcompletion")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractRequest_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"abab5.5-chat"}`), "/v1/text/chatcompletion")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractResponse_NativeFormat(t *testing.T) {
	body := []byte(`{
		"choices": [
			{"message": {"sender_type": "BOT", "text": "Hello from MiniMax!"}}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/text/chatcompletion")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello from MiniMax!" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractResponse_CompatFormat(t *testing.T) {
	body := []byte(`{
		"choices": [
			{"message": {"role": "assistant", "content": "Hello from MiniMax!"}}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/text/chatcompletion_v2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello from MiniMax!" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/v1/text/chatcompletion")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractResponse_MissingChoices(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"reply":"hello"}`), "/v1/text/chatcompletion")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractStreamChunk_NativeFormat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"text":"Hello"}}]}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/text/chatcompletion")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractStreamChunk_CompatFormat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/text/chatcompletion_v2")
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
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/text/chatcompletion")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("expected 0 segments for empty delta, got %d", len(nc.Segments))
	}
}

func TestExtractStreamChunk_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/v1/text/chatcompletion")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}
