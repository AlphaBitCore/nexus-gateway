package normalize

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// reproEvent describes one captured agent event we replay through the
// shared registry to verify Normalize() picks the right adapter.
type reproEvent struct {
	name        string
	host        string
	endpoint    string
	contentType string
	stream      bool
	// adapterType simulates what PolicyResolver would return as the
	// resolved adapter ID (agent's forward_handler passes adapter.ID()
	// to runtimeNormalize's Meta.AdapterType). Empty means "PolicyResolver
	// returned no adapter" — registry falls back to Tier 2+3 only.
	adapterType     string
	reqFile         string
	respFile        string
	wantReqAdapter  string // expected adapter ID in request normalized
	wantRespAdapter string // expected adapter ID in response normalized
	// wantProtocol overrides the Protocol assertion when the adapter
	// delegates to a shared codec (empty = same as the adapter ID).
	wantProtocol string
}

// TestRepro_BrowserCaptures replays four captured payloads (chatgpt + claude.ai
// request + response) through the canonical Tier 1+2+3 registry the
// agent / cp / ai-gateway / hub all share. Bug surfaced 2026-05-25 from
// the live agent capture: chatgpt.com request normalized as chatgpt-web
// (good) but response landed NULL (bad); claude.ai both request + response
// landed as generic-http (bad — should be claude-web both sides).
func TestRepro_BrowserCaptures(t *testing.T) {
	cases := []reproEvent{
		{
			name:           "chatgpt-request",
			host:           "chatgpt.com",
			endpoint:       "/backend-api/f/conversation",
			contentType:    "application/json",
			stream:         false,
			adapterType:    "chatgpt-web",
			reqFile:        "chatgptweb-req.json",
			wantReqAdapter: "chatgpt-web",
		},
		{
			name:            "chatgpt-response",
			host:            "chatgpt.com",
			endpoint:        "/backend-api/f/conversation",
			contentType:     "text/event-stream",
			stream:          true,
			adapterType:     "chatgpt-web",
			respFile:        "chatgptweb-resp.sse",
			wantRespAdapter: "chatgpt-web",
		},
		{
			name:           "claudeai-request",
			host:           "claude.ai",
			endpoint:       "/api/organizations/91bafac6-6120-4d56-964e-6459c4f7cd5a/chat_conversations/70d7e000-17df-4353-ba7f-f516e8fb0990/completion",
			contentType:    "application/json",
			stream:         false,
			adapterType:    "claude-web",
			reqFile:        "claudeweb-req.json",
			wantReqAdapter: "claude-web",
		},
		{
			name:            "claudeai-response",
			host:            "claude.ai",
			endpoint:        "/api/organizations/91bafac6-6120-4d56-964e-6459c4f7cd5a/chat_conversations/70d7e000-17df-4353-ba7f-f516e8fb0990/completion",
			contentType:     "text/event-stream",
			stream:          true,
			adapterType:     "claude-web",
			respFile:        "claudeweb-resp.sse",
			wantRespAdapter: "claude-web",
			// The response wire is standard Anthropic Messages SSE: the
			// claude-web adapter delegates to the shared codec, which
			// keeps its own Protocol while DetectedSpec carries the
			// per-host provenance.
			wantProtocol: "anthropic-messages",
		},
	}

	reg := BuildRegistry()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file := c.reqFile
			direction := core.DirectionRequest
			if c.respFile != "" {
				file = c.respFile
				direction = core.DirectionResponse
			}
			raw, err := os.ReadFile(filepath.Join("testdata", file))
			if err != nil {
				t.Fatalf("read testdata: %v", err)
			}

			meta := core.Meta{
				AdapterType:  c.adapterType,
				ContentType:  c.contentType,
				Direction:    direction,
				EndpointPath: c.endpoint,
				Stream:       c.stream,
			}

			payload, err := reg.Normalize(context.Background(), raw, meta)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}

			// Pretty-print payload so we see exactly what the registry
			// produced — including Kind, Protocol, Confidence,
			// Messages, AssistantText, etc.
			pretty, _ := json.MarshalIndent(payload, "", "  ")
			t.Logf("payload:\n%s", pretty)

			t.Logf("ASSERT: kind=%q protocol=%q confidence=%v",
				payload.Kind, payload.Protocol, payload.Confidence)

			wantAdapter := c.wantReqAdapter
			if direction == core.DirectionResponse {
				wantAdapter = c.wantRespAdapter
			}
			if payload.DetectedSpec != wantAdapter {
				t.Errorf("DetectedSpec = %q, want %q (kind=%s confidence=%v)",
					payload.DetectedSpec, wantAdapter, payload.Kind, payload.Confidence)
			}
			wantProtocol := c.wantProtocol
			if wantProtocol == "" {
				wantProtocol = wantAdapter
			}
			if payload.Protocol != wantProtocol {
				t.Errorf("Protocol = %q, want %q (kind=%s confidence=%v)",
					payload.Protocol, wantProtocol, payload.Kind, payload.Confidence)
			}
		})
	}
}
