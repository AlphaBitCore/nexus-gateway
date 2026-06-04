package deepseek_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/deepseek"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestDeepSeek_Spec_Valid(t *testing.T) {
	s := deepseek.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatDeepSeek {
		t.Errorf("format %q", s.Format)
	}
}

func TestDeepSeek_Transport_BuildURL(t *testing.T) {
	s := deepseek.NewSpec(slog.Default())
	got, err := s.Transport.BuildURL(
		provcore.CallTarget{BaseURL: "https://api.deepseek.com"},
		typology.WireShapeOpenAIChat, false,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got != "https://api.deepseek.com/v1/chat/completions" {
		t.Errorf("got %q", got)
	}
}
