package bedrock

import (
	"bytes"
	"log/slog"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// This wire must not receive a prompt-cache marker.
//
// Bedrock's codec delegates the whole canonical → Anthropic-Messages
// translation to the Anthropic codec, which is exactly why the marker would
// ride along for free — and exactly why it needs a gate. AWS documents its
// InvokeModel Claude integration answering 400 for a root `cache_control`,
// nothing in this repository has been measured against a live Bedrock
// endpoint, and an unverified field on a wire documented to reject it turns
// every request into a 400 rather than degrading quietly.
//
// The assertion is on the EMITTED BYTES rather than on the flag, so it fails
// whichever way the exclusion is lost: someone deleting the line in
// EncodeRequest, or the Anthropic codec growing a second place that writes the
// marker. Enabling this wire means running the three-arm probe against a live
// endpoint first and changing this test in the same commit.
func TestBedrock_PromptCacheMarkerNeverReachesTheWire(t *testing.T) {
	canonical := []byte(`{"model":"anthropic.claude-sonnet-4-6-v1:0","max_tokens":16,` +
		`"messages":[{"role":"system","content":"stable operator playbook"},` +
		`{"role":"user","content":"hello"}]}`)

	res, err := NewCodec(slog.Default()).EncodeRequest(
		typology.WireShapeBedrockConverse, canonical,
		provcore.CallTarget{
			ProviderModelID: "anthropic.claude-sonnet-4-6-v1:0",
			MaxOutputTokens: 64000,
			// The operator has markers ON for this provider. The wire, not the
			// setting, is what excludes them.
			PromptCacheMarkers: true,
		})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if bytes.Contains(res.Body, []byte(`"cache_control"`)) {
		t.Fatalf("a prompt-cache marker reached the Bedrock wire, which AWS documents as a 400: %s", res.Body)
	}
	if res.PromptCacheMarked {
		t.Error("PromptCacheMarked must be false — nothing was marked")
	}
	// The delegation itself must still be intact, or this test would pass for
	// the wrong reason (an EncodeRequest that produced nothing at all).
	if !bytes.Contains(res.Body, []byte(`"anthropic_version":"`+anthropicVersion+`"`)) {
		t.Fatalf("Bedrock's own body rules must still apply: %s", res.Body)
	}
}
