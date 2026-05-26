package xai_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/xai"
)

func TestXai_Spec_Valid(t *testing.T) {
	s := xai.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatXai {
		t.Errorf("format=%q", s.Format)
	}
}

func TestXai_Transport_BuildURL(t *testing.T) {
	s := xai.NewSpec(slog.Default())
	got, err := s.Transport.BuildURL(
		provcore.CallTarget{BaseURL: "https://api.x.ai"},
		typology.WireShapeOpenAIChat, false,
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "https://api.x.ai/v1/chat/completions" {
		t.Errorf("got=%q", got)
	}
}

func TestXai_NilLogger(t *testing.T) {
	if !xai.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
