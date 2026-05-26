package moonshot_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/moonshot"
)

func TestMoonshot_Spec_Valid(t *testing.T) {
	s := moonshot.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatMoonshot {
		t.Errorf("format=%q", s.Format)
	}
}

func TestMoonshot_NilLogger(t *testing.T) {
	if !moonshot.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
