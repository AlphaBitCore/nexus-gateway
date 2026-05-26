package groq_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/groq"
)

func TestGroq_Spec_Valid(t *testing.T) {
	s := groq.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatGroq {
		t.Errorf("format=%q", s.Format)
	}
}

func TestGroq_NilLogger(t *testing.T) {
	if !groq.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
