package mistral_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/mistral"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestMistral_Spec_Valid(t *testing.T) {
	s := mistral.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatMistral {
		t.Errorf("format=%q", s.Format)
	}
}

func TestMistral_Transport_BuildURL(t *testing.T) {
	s := mistral.NewSpec(slog.Default())
	got, err := s.Transport.BuildURL(
		provcore.CallTarget{BaseURL: "https://api.mistral.ai"},
		typology.WireShapeOpenAIChat, false,
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "https://api.mistral.ai/v1/chat/completions" {
		t.Errorf("got=%q", got)
	}
}

func TestMistral_NilLogger(t *testing.T) {
	if !mistral.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
