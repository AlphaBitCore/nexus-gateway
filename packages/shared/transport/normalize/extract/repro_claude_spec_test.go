package extract

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepro_ClaudeWebSpecScore directly runs ScoreChatSpec on the real
// claude.ai request body to see why my repro_browser_test.go shows
// confidence=0 even though the spec's SignatureFields list six fields
// the body clearly contains (parent_message_uuid, rendering_mode,
// personalized_styles, sync_sources, timezone, locale).
func TestRepro_ClaudeWebSpecScore(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "claudeweb-req.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	t.Logf("body size=%d, first 200 bytes: %s", len(raw), string(raw[:200]))

	specIDs := []string{"claude-web", "anthropic-messages", "anthropic-completions-legacy"}
	for _, id := range specIDs {
		spec := ChatSpecByID(id)
		if spec == nil {
			t.Errorf("ChatSpecByID(%q) returned nil — spec not registered!", id)
			continue
		}
		d := ScoreChatSpec(raw, *spec)
		t.Logf("%-32s → confidence=%.3f userPrompts=%d msgRoles=%d model=%q",
			id, d.Confidence, len(d.UserPrompts), len(d.MessageRoles), d.Model)
	}
}
