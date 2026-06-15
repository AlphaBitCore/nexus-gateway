package claudeweb

import (
	"context"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestNormalize_RealClaudeWebRequestShape exercises the real claude.ai
// request shape (single-prompt body against the
// /api/organizations/.../chat_conversations/.../completion endpoint).
// The body carries no messages[] array — only the dedicated claude-web
// spec's single-prompt shape + signature fields claim Tier 1.
func TestNormalize_RealClaudeWebRequestShape(t *testing.T) {
	body := []byte(`{
		"prompt": "dododododo",
		"model": "claude-opus-4-7",
		"parent_message_uuid": "019e2b0f-bb13-7dde-8d32-eeff6892fcd1",
		"rendering_mode": "messages",
		"turn_message_uuids": [],
		"personalized_styles": {},
		"sync_sources": [],
		"timezone": "America/Los_Angeles",
		"locale": "en-US",
		"files": [],
		"attachments": [],
		"tools": [{"name": "show_widget", "description": "Render a widget"}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/organizations/abc/chat_conversations/xyz/completion",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("Kind: %v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != adapterID {
		t.Errorf("DetectedSpec: %q want %q", payload.DetectedSpec, adapterID)
	}
	if payload.Model != "claude-opus-4-7" {
		t.Errorf("Model: %q want claude-opus-4-7", payload.Model)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("messages: %d want 1 (%+v)", len(payload.Messages), payload.Messages)
	}
	got := payload.Messages[0].Content[0].Text
	if !strings.Contains(got, "dododododo") {
		t.Errorf("user prompt: %q want to contain 'dododododo'", got)
	}
	if payload.Messages[0].Role != normalize.RoleUser {
		t.Errorf("role: %v want user", payload.Messages[0].Role)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("confidence: %v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_NotClaudeWeb_FallsThrough makes sure a body that isn't
// claude-web shape (no prompt, no signature fields) returns
// ErrUnsupported so the Coordinator falls through to Tier 2 / Tier 3.
func TestNormalize_NotClaudeWeb_FallsThrough(t *testing.T) {
	body := []byte(`{"foo": "bar", "count": 42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported")
	}
}
