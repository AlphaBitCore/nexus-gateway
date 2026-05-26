package perplexity_test

import (
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/perplexity"
)

func TestPerplexity_Spec_Valid(t *testing.T) {
	s := perplexity.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatPerplexity {
		t.Errorf("format=%q", s.Format)
	}
}

func TestPerplexity_NilLogger(t *testing.T) {
	if !perplexity.NewSpec(nil).Valid() {
		t.Errorf("spec invalid with nil logger")
	}
}
