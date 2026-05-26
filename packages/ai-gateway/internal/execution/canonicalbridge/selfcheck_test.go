package canonicalbridge

import (
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

func TestBridge_SelfCheck(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	if err := b.SelfCheck(); err != nil {
		t.Fatal(err)
	}
}

func TestBridge_SelfCheck_failsWhenTargetCodecMissing(t *testing.T) {
	log := slog.Default()
	full := provbuiltins.SchemaCodecs(log)
	partial := map[provcore.Format]provcore.SchemaCodec{
		provcore.FormatOpenAI:    full[provcore.FormatOpenAI],
		provcore.FormatAnthropic: full[provcore.FormatAnthropic],
	}
	b := New(partial)
	err := b.SelfCheck()
	if err == nil {
		t.Fatal("expected error when a ChatRoutable target codec is missing")
	}
}
