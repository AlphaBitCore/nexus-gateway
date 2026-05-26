package together_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/together"
)

func TestTogether_Spec_Valid(t *testing.T) {
	s := together.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatTogether {
		t.Errorf("format=%q", s.Format)
	}
}

func TestTogether_NilLogger(t *testing.T) {
	if !together.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
