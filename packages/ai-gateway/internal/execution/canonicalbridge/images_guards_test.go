package canonicalbridge

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// ValidateImagesIngressGuards runs on the passthrough leg, where the ingress
// body goes upstream verbatim and IngressImagesToCanonical never runs. What it
// must carry over is the SPEND bound — `n` multiplies per-image billing from a
// single request — and nothing else: the canonical shape rules exist so the
// cross-format codec has something it can translate, and on this leg the
// provider's own opinion of its body is the one that counts.
func TestValidateImagesIngressGuards(t *testing.T) {
	b := New(nil)
	ct := provcore.CallTarget{Format: provcore.FormatOpenAI, ProviderModelID: "gpt-image-1-mini"}

	t.Run("the spend bound is carried over", func(t *testing.T) {
		for _, body := range []string{
			`{"prompt":"x","n":5}`,
			`{"prompt":"x","n":10}`,
			`{"prompt":"x","n":0}`,
			`{"prompt":"x","n":-1}`,
			`{"prompt":"x","n":1.5}`,
			`{"prompt":"x","n":"2"}`,
		} {
			err := b.ValidateImagesIngressGuards(provcore.FormatOpenAI, []byte(body), ct)
			if err == nil {
				t.Errorf("%s was accepted; the upstream would have billed for it", body)
				continue
			}
			if !strings.Contains(err.Error(), `"n"`) {
				t.Errorf("%s: error does not name the field: %v", body, err)
			}
		}
	})

	t.Run("what the upstream may legitimately serve is left alone", func(t *testing.T) {
		for _, body := range []string{
			`{"prompt":"x"}`,
			`{"prompt":"x","n":1}`,
			`{"prompt":"x","n":4}`,
			// Shape rules belong to the canonical lane. On a passthrough leg
			// rejecting a body the provider would have served is a regression,
			// not a guard — the same line the rerank sibling draws.
			`{"n":2}`,
			`{"prompt":"x","n":2,"nexus":{"anything":true}}`,
			`{"prompt":"x","n":2,"size":"1024x1024","response_format":"b64_json"}`,
		} {
			if err := b.ValidateImagesIngressGuards(provcore.FormatOpenAI, []byte(body), ct); err != nil {
				t.Errorf("%s was refused: %v", body, err)
			}
		}
	})

	t.Run("a non-OpenAI ingress has no images canonical at all", func(t *testing.T) {
		if err := b.ValidateImagesIngressGuards(provcore.FormatAnthropic, []byte(`{"prompt":"x"}`), ct); err == nil {
			t.Error("an Anthropic ingress was accepted on the image lane")
		}
	})
}
