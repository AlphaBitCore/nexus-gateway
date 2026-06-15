package extract

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestRepro_ClaudeWebSpecScore pins ScoreChatSpec against the real
// claude.ai request body. It guards two regressions: (1) the claude-web
// spec must out-score the flat-prompt legacy spec on a browser body that
// carries claude.ai-specific signature fields (parent_message_uuid,
// rendering_mode, personalized_styles, sync_sources, timezone, locale),
// and (2) the extractor must recover the user prompt and model from the
// flat claude.ai `prompt`/`model` shape rather than the messages array.
func TestRepro_ClaudeWebSpecScore(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "claudeweb-req.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	score := func(id string) ChatDetection {
		spec := ChatSpecByID(id)
		if spec == nil {
			t.Fatalf("ChatSpecByID(%q) returned nil — spec not registered", id)
		}
		return ScoreChatSpec(raw, *spec)
	}

	claudeWeb := score("claude-web")
	legacy := score("openai-completions-legacy")

	// claude-web is the strongest match for a claude.ai browser body.
	if claudeWeb.Confidence <= legacy.Confidence {
		t.Errorf("claude-web confidence %.3f should exceed legacy %.3f",
			claudeWeb.Confidence, legacy.Confidence)
	}

	// Pinned claude-web detection: the body's flat `prompt`/`model` fields
	// resolve to exactly one user prompt and the claude-opus model.
	if got, want := claudeWeb.Confidence, 0.600; math.Abs(got-want) > 1e-9 {
		t.Errorf("claude-web confidence = %.3f, want %.3f", got, want)
	}
	if got := claudeWeb.UserPrompts; len(got) != 1 || got[0] != "hello" {
		t.Errorf("claude-web UserPrompts = %#v, want [\"hello\"]", got)
	}
	if got, want := len(claudeWeb.MessageRoles), 1; got != want {
		t.Errorf("claude-web MessageRoles len = %d, want %d", got, want)
	}
	if got, want := claudeWeb.Model, "claude-opus-4-7"; got != want {
		t.Errorf("claude-web Model = %q, want %q", got, want)
	}

}
