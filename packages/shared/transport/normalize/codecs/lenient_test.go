package codecs

import (
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

type lenientProbe struct {
	Model       string          `json:"model"`
	Messages    []lenientMsg    `json:"messages"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	hidden      string          //nolint:unused // exercises the unexported-field skip
}

type lenientMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TestDecodeLenient pins the business contract: a mistyped optional scalar
// must NOT erase the decodable fields (especially messages — the audit
// record's content), while genuinely unrecoverable input still errors.
func TestDecodeLenient(t *testing.T) {
	t.Run("clean body decodes fully", func(t *testing.T) {
		var p lenientProbe
		raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`
		if err := decodeLenient([]byte(raw), &p); err != nil {
			t.Fatalf("err=%v", err)
		}
		if p.Model != "m" || len(p.Messages) != 1 || p.Messages[0].Content != "hi" {
			t.Fatalf("full decode lost data: %+v", p)
		}
		if p.Temperature == nil || *p.Temperature != 0.7 {
			t.Fatalf("temperature lost: %+v", p.Temperature)
		}
	})

	t.Run("mistyped scalar keeps messages (the bug)", func(t *testing.T) {
		var p lenientProbe
		raw := `{"model":"m","messages":[{"role":"user","content":"secret prompt"}],"temperature":"0.7"}`
		if err := decodeLenient([]byte(raw), &p); err != nil {
			t.Fatalf("lenient decode must recover; err=%v", err)
		}
		if len(p.Messages) != 1 || p.Messages[0].Content != "secret prompt" {
			t.Fatalf("messages erased — the audit-loss bug: %+v", p)
		}
		if p.Model != "m" {
			t.Fatalf("model lost: %+v", p)
		}
		if p.Temperature != nil {
			t.Fatalf("mistyped temperature must be dropped, not guessed: %v", *p.Temperature)
		}
	})

	t.Run("mistyped int scalar", func(t *testing.T) {
		var p lenientProbe
		raw := `{"messages":[{"role":"user","content":"x"}],"max_tokens":"100"}`
		if err := decodeLenient([]byte(raw), &p); err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(p.Messages) != 1 || p.MaxTokens != nil {
			t.Fatalf("recover wrong: %+v", p)
		}
	})

	t.Run("non-object input surfaces original error", func(t *testing.T) {
		var p lenientProbe
		if err := decodeLenient([]byte(`"just a string"`), &p); err == nil {
			t.Fatal("want error for non-object input")
		}
	})

	t.Run("object with zero recoverable fields errors", func(t *testing.T) {
		var p lenientProbe
		// every present field mistyped
		if err := decodeLenient([]byte(`{"model":123,"messages":"nope"}`), &p); err == nil {
			t.Fatal("want error when nothing recovered")
		}
	})

	t.Run("codec end-to-end: mistyped temperature keeps prompt text", func(t *testing.T) {
		// The original bug: captured third-party traffic with one mistyped
		// optional scalar produced a "partial" normalized record with ZERO
		// message content — prompt text silently erased from the audit
		// record. The codec must now still emit the messages.
		n := &OpenAIChatNormalizer{}
		raw := `{"model":"gpt-4o","messages":[{"role":"user","content":"the captured secret prompt"}],"temperature":"0.7"}`
		payload, err := n.normalizeRequest([]byte(raw), core.Meta{Direction: core.DirectionRequest})
		if err != nil {
			t.Fatalf("normalizeRequest must recover: %v", err)
		}
		found := false
		for _, m := range payload.Messages {
			for _, c := range m.Content {
				if c.Text == "the captured secret prompt" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("prompt text erased from normalized payload: %+v", payload.Messages)
		}
	})

	t.Run("null and absent fields skipped", func(t *testing.T) {
		var p lenientProbe
		raw := `{"messages":[{"role":"user","content":"x"}],"temperature":null,"stop":["a"],"model":7}`
		if err := decodeLenient([]byte(raw), &p); err != nil {
			t.Fatalf("err=%v", err)
		}
		if p.Temperature != nil || string(p.Stop) != `["a"]` || p.Model != "" {
			t.Fatalf("skip semantics wrong: %+v", p)
		}
	})
}
