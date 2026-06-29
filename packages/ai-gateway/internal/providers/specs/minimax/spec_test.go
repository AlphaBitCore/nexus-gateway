package minimax_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/minimax"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestMiniMax_OpenAICompatURL(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://api.minimax.io"}, typology.WireShapeOpenAIChat, false)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got != "https://api.minimax.io/v1/chat/completions" {
		t.Errorf("got %q", got)
	}
}

// TestMiniMax_EmptyBaseURLErrors pins that an empty BaseURL errors out.
// Per the project's "baseUrl must be origin-only" rule the seeded provider
// template ships an explicit baseUrl; no implicit fallback is provided so a
// misconfigured provider fails loudly instead of silently routing to one
// region.
func TestMiniMax_EmptyBaseURLErrors(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	_, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeOpenAIChat, false)
	if err == nil {
		t.Fatal("expected error for empty BaseURL")
	}
	if !strings.Contains(err.Error(), "BaseURL is empty") {
		t.Errorf("err: %v", err)
	}
}

// TestMiniMax_GroupIdForwarded confirms minimax.groupId is appended as a
// query param on chat completions (some MiniMax tenants still require it
// for billing attribution).
func TestMiniMax_GroupIdForwarded(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL: "https://api.minimax.io",
			Extras:  map[string]string{"minimax.groupId": "12345"},
		},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(got, "/v1/chat/completions") {
		t.Errorf("expected OpenAI-compat path, got %q", got)
	}
	if !strings.Contains(got, "GroupId=12345") {
		t.Errorf("GroupId query missing: %s", got)
	}
}

// TestMiniMax_NativePathRetired pins the audit decision: the legacy
// chatcompletion_pro native shape is intentionally not routable. Any
// caller that sets minimax.surface=native receives the same OpenAI-compat
// URL as everyone else (there is no native ingress route; see main.go).
func TestMiniMax_NativePathRetired(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL: "https://api.minimax.io",
			Extras:  map[string]string{"minimax.surface": "native"},
		},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if strings.Contains(got, "chatcompletion_pro") {
		t.Errorf("legacy native path must not surface: %q", got)
	}
}

func TestMiniMax_ApplyAuth(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "tok"}); err != nil {
		t.Fatalf("%v", err)
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("Authorization: %q", req.Header.Get("Authorization"))
	}
}
