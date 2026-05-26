package fireworks_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/fireworks"
)

func TestFireworks_Spec_Valid(t *testing.T) {
	s := fireworks.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatFireworks {
		t.Errorf("format=%q", s.Format)
	}
}

func TestFireworks_NilLogger(t *testing.T) {
	if !fireworks.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
