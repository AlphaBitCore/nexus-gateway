package openai_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The anti-drift gate for a defect that has now appeared three times: the
// normalize codec treats something as the caller's text while the compliance
// adapter does not see it, so the value is persisted and forwarded with no
// policy ever reading it. It showed up in TTS `instructions`, in Cohere's
// object-shaped rerank documents, and here in chat content parts spelled with
// any type but "text".
//
// The invariant is one-directional on purpose: every piece of USER text the
// codec recognises must appear in the adapter's scan segments. The reverse is
// allowed — the adapter may scan more than the codec files as prose, and
// over-scanning is safe.
//
// Each case is a body whose scannable prose is a unique sentinel, so a miss
// names the exact shape that slipped through rather than a count.
func TestScanCoversEveryTextTheCodecRecognises(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		sentinels []string
	}{
		{
			name:      "plain string content",
			body:      `{"model":"m","messages":[{"role":"user","content":"SENTINEL_PLAIN"}]}`,
			sentinels: []string{"SENTINEL_PLAIN"},
		},
		{
			name:      "text part",
			body:      `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"SENTINEL_TEXT"}]}]}`,
			sentinels: []string{"SENTINEL_TEXT"},
		},
		{
			name:      "responses-vocabulary part on the chat wire",
			body:      `{"model":"m","messages":[{"role":"user","content":[{"type":"input_text","text":"SENTINEL_INPUT_TEXT"}]}]}`,
			sentinels: []string{"SENTINEL_INPUT_TEXT"},
		},
		{
			name:      "an unknown future part that still carries prose",
			body:      `{"model":"m","messages":[{"role":"user","content":[{"type":"some_future_type","text":"SENTINEL_FUTURE"}]}]}`,
			sentinels: []string{"SENTINEL_FUTURE"},
		},
		{
			name:      "system and user together",
			body:      `{"model":"m","messages":[{"role":"system","content":"SENTINEL_SYSTEM"},{"role":"user","content":[{"type":"text","text":"SENTINEL_USER"}]}]}`,
			sentinels: []string{"SENTINEL_SYSTEM", "SENTINEL_USER"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// What the codec files as the caller's text.
			payload, err := codecs.NewOpenAIChatNormalizer().Normalize(
				context.Background(), []byte(tc.body), core.Meta{Direction: core.DirectionRequest})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			var codecText []string
			for _, m := range payload.Messages {
				for _, b := range m.Content {
					if b.Type == core.ContentText && strings.Contains(b.Text, "SENTINEL_") {
						codecText = append(codecText, b.Text)
					}
				}
			}

			// What the compliance pipeline is handed.
			nc, err := (&openai.Adapter{}).ExtractRequest(context.Background(), []byte(tc.body), "/v1/chat/completions")
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			scanned := strings.Join(nc.Segments, "\x00")

			for _, want := range tc.sentinels {
				if !strings.Contains(scanned, want) {
					t.Errorf("%q is text the codec files and the scanner never sees — it is persisted and forwarded with no policy reading it; codec had %q, scanner had %q",
						want, codecText, nc.Segments)
				}
			}
		})
	}
}
