package huggingface_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/huggingface"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestHuggingFace_Spec_Valid(t *testing.T) {
	s := huggingface.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatHuggingFace {
		t.Errorf("format=%q", s.Format)
	}
}

func TestHuggingFace_Transport_BuildURL(t *testing.T) {
	s := huggingface.NewSpec(slog.Default())
	got, err := s.Transport.BuildURL(
		provcore.CallTarget{BaseURL: "https://api-inference.huggingface.co"},
		typology.WireShapeOpenAIChat, false,
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "https://api-inference.huggingface.co/v1/chat/completions" {
		t.Errorf("got=%q", got)
	}
}

func TestHuggingFace_NilLogger(t *testing.T) {
	if !huggingface.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
