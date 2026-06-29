package claudeweb

import (
	"context"
	"github.com/goccy/go-json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestRepro_ClaudeWebAdapterNormalize replays the live agent-captured
// claude.ai bodies (saved under packages/shared/transport/normalize/
// testdata/) directly through Adapter.Normalize so we see whether
// Tier 1 returns a payload above the registry's 0.7 threshold, or
// falls back to ErrUnsupported letting Tier 2/3 take over.
func TestRepro_ClaudeWebAdapterNormalize(t *testing.T) {
	cases := []struct {
		name      string
		file      string
		direction core.Direction
		ct        string
	}{
		{
			name:      "request",
			file:      "claudeweb-req.json",
			direction: core.DirectionRequest,
			ct:        "application/json",
		},
		{
			name:      "response",
			file:      "claudeweb-resp.sse",
			direction: core.DirectionResponse,
			ct:        "text/event-stream",
		},
	}

	a := &Adapter{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "transport", "normalize", "testdata", c.file))
			if err != nil {
				t.Fatalf("read testdata: %v", err)
			}
			meta := core.Meta{
				AdapterType:  "claude-web",
				ContentType:  c.ct,
				Direction:    c.direction,
				EndpointPath: "/api/organizations/x/chat_conversations/y/completion",
				Stream:       c.direction == core.DirectionResponse,
			}
			payload, err := a.Normalize(context.Background(), raw, meta)
			pretty, _ := json.MarshalIndent(payload, "", "  ")
			t.Logf("err=%v\npayload:\n%s", err, pretty)
			t.Logf("ASSERT: kind=%q protocol=%q confidence=%v detectedSpec=%q",
				payload.Kind, payload.Protocol, payload.Confidence, payload.DetectedSpec)
		})
	}
}
