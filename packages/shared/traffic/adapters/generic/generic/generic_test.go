package generic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestConfigure_Valid(t *testing.T) {
	a := &Adapter{}
	err := a.Configure(map[string]any{
		"requestPaths":  []any{"messages.#.content"},
		"responsePaths": []any{"choices.#.message.content"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(a.requestPaths) != 1 {
		t.Errorf("expected 1 request path, got %d", len(a.requestPaths))
	}
}

func TestConfigure_NoPaths(t *testing.T) {
	a := &Adapter{}
	err := a.Configure(map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty config")
	}
}

func TestConfigure_EmptyPath(t *testing.T) {
	a := &Adapter{}
	err := a.Configure(map[string]any{
		"requestPaths": []any{""},
	})
	if err == nil {
		t.Fatal("expected error for empty path string")
	}
}

func TestExtractRequest(t *testing.T) {
	a := &Adapter{}
	_ = a.Configure(map[string]any{
		"requestPaths": []any{"messages.#.content", "prompt"},
	})

	body := []byte(`{"messages":[{"content":"hello"},{"content":"world"}],"prompt":"test"}`)
	nc, err := a.ExtractRequest(context.Background(), body, "/api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// messages.#.content returns array ["hello","world"], prompt returns "test"
	if len(nc.Segments) != 3 {
		t.Fatalf("expected 3 segments, got %d: %v", len(nc.Segments), nc.Segments)
	}
}

func TestExtractRequest_SingleValue(t *testing.T) {
	a := &Adapter{}
	_ = a.Configure(map[string]any{
		"requestPaths": []any{"text"},
	})

	body := []byte(`{"text":"single value"}`)
	nc, err := a.ExtractRequest(context.Background(), body, "/api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "single value" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractRequest_NoMatch(t *testing.T) {
	a := &Adapter{}
	_ = a.Configure(map[string]any{
		"requestPaths": []any{"nonexistent.path"},
	})

	body := []byte(`{"other":"data"}`)
	nc, err := a.ExtractRequest(context.Background(), body, "/api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("expected 0 segments, got %d", len(nc.Segments))
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_ = a.Configure(map[string]any{
		"requestPaths": []any{"text"},
	})

	_, err := a.ExtractRequest(context.Background(), []byte("not json"), "/api")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractResponse(t *testing.T) {
	a := &Adapter{}
	_ = a.Configure(map[string]any{
		"requestPaths":  []any{"input"},
		"responsePaths": []any{"output.text"},
	})

	body := []byte(`{"output":{"text":"response content"}}`)
	nc, err := a.ExtractResponse(context.Background(), body, "/api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "response content" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractStreamChunk(t *testing.T) {
	a := &Adapter{}
	_ = a.Configure(map[string]any{
		"requestPaths":     []any{"input"},
		"streamDeltaPaths": []any{"delta.content"},
	})

	chunk := []byte(`{"delta":{"content":"hello"}}`)
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractNoPaths(t *testing.T) {
	a := &Adapter{}
	a.requestPaths = nil

	nc, err := a.ExtractRequest(context.Background(), []byte(`{}`), "/api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("expected empty segments")
	}
}

func TestID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "generic-jsonpath" {
		t.Errorf("ID = %q", a.ID())
	}
}

// Configure — error wrapping per slice key

// TestConfigure_RequestPathsWrongType covers the requestPaths error
// wrapping branch (Configure line 36-38).
func TestConfigure_RequestPathsWrongType(t *testing.T) {
	a := &Adapter{}
	err := a.Configure(map[string]any{
		"requestPaths": "not an array",
	})
	if err == nil || !strings.Contains(err.Error(), "requestPaths") {
		t.Fatalf("err=%v want wrapped requestPaths error", err)
	}
}

// TestConfigure_ResponsePathsWrongType covers responsePaths error
// wrapping (Configure line 40-42).
func TestConfigure_ResponsePathsWrongType(t *testing.T) {
	a := &Adapter{}
	err := a.Configure(map[string]any{
		"requestPaths":  []any{"messages.#.content"},
		"responsePaths": 123,
	})
	if err == nil || !strings.Contains(err.Error(), "responsePaths") {
		t.Fatalf("err=%v want wrapped responsePaths error", err)
	}
}

// TestConfigure_StreamDeltaPathsWrongType covers streamDeltaPaths
// error wrapping (Configure line 44-46).
func TestConfigure_StreamDeltaPathsWrongType(t *testing.T) {
	a := &Adapter{}
	err := a.Configure(map[string]any{
		"requestPaths":     []any{"messages.#.content"},
		"streamDeltaPaths": map[string]any{"oops": true},
	})
	if err == nil || !strings.Contains(err.Error(), "streamDeltaPaths") {
		t.Fatalf("err=%v want wrapped streamDeltaPaths error", err)
	}
}

// extractStringSlice — non-array + non-string-item branches

// TestConfigure_PathsItemNotString covers extractStringSlice line 137-139
// (an item in the array is not a string).
func TestConfigure_PathsItemNotString(t *testing.T) {
	a := &Adapter{}
	err := a.Configure(map[string]any{
		"requestPaths": []any{"messages.#.content", 42},
	})
	if err == nil || !strings.Contains(err.Error(), "requestPaths[1]") {
		t.Fatalf("err=%v want item-not-string error", err)
	}
}

// Normalize (Tier-1 multi-spec dispatch)

// TestNormalize_OpenAIChat exercises the openai-chat branch of the
// adapter's codec walk + consumer-web fallback.
func TestNormalize_OpenAIChat(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4",
		"messages":[
			{"role":"system","content":"You are a helpful assistant."},
			{"role":"user","content":"hello"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "generic-jsonpath",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "generic-jsonpath" {
		t.Errorf("DetectedSpec=%q want generic-jsonpath", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape verifies the ErrUnsupported fall-through.
func TestNormalize_UnrecognisedShape(t *testing.T) {
	body := []byte(`{"random":"unrelated","keys":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "generic-jsonpath",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}
