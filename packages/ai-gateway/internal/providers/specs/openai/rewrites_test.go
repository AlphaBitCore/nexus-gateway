package openai

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// TestNewIdentityCodec_BridgeSurface drives the bridge-level constructor
// with the bridge-level contract — the exact pair sibling adapters (azure)
// wire — and asserts the reasoning quirks land through it: the rename, the
// strip, and their reports. A bridge re-export that drifted from the codec
// package would strand every sibling on stale behaviour.
func TestNewIdentityCodec_BridgeSurface(t *testing.T) {
	t.Parallel()

	c := NewIdentityCodec(OpenAIContract())
	res, err := c.RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"o3","messages":[],"max_tokens":50,"temperature":0.7}`),
		provcore.CallTarget{ProviderModelID: "o3"}, false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if gjson.GetBytes(res.Body, "max_tokens").Exists() {
		t.Errorf("max_tokens must be renamed for o3: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "max_completion_tokens").Int() != 50 {
		t.Errorf("max_completion_tokens must carry the value: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "temperature").Exists() {
		t.Errorf("temperature must be stripped for o3: %s", res.Body)
	}
	want := []string{"max_tokens→max_completion_tokens", "temperature→removed"}
	if len(res.Rewrites) != 2 || res.Rewrites[0] != want[0] || res.Rewrites[1] != want[1] {
		t.Errorf("rewrites: got %v, want %v", res.Rewrites, want)
	}
}

// TestNewIdentityCodec_EmptyContract pins the zero-Contract path the
// quirk-free siblings construct: stamp only, nothing stripped, nothing
// reported.
func TestNewIdentityCodec_EmptyContract(t *testing.T) {
	t.Parallel()

	c := NewIdentityCodec(Contract{})
	res, err := c.RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"alias","messages":[],"max_tokens":50,"temperature":0.7}`),
		provcore.CallTarget{ProviderModelID: "o3"}, false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if gjson.GetBytes(res.Body, "model").String() != "o3" {
		t.Errorf("stamp must land: %s", res.Body)
	}
	if !gjson.GetBytes(res.Body, "max_tokens").Exists() || !gjson.GetBytes(res.Body, "temperature").Exists() {
		t.Errorf("an empty contract must not strip anything: %s", res.Body)
	}
	if len(res.Rewrites) != 0 {
		t.Errorf("nothing to report: %v", res.Rewrites)
	}
}
