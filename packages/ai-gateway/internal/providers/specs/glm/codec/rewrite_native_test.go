package codec

import (
	"bytes"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// GLM shares the OpenAI-family wire, so its native differential must be the
// family-wide one — same stamp, same streaming usage injection — or GLM
// streams silently lose usage extraction while every sibling keeps it.
func TestGLMRewriteNative_FamilyDifferential(t *testing.T) {
	target := provcore.CallTarget{ProviderModelID: "glm-5"}
	res, err := New().RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"glm-alias","messages":[]}`), target, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "model").String() != "glm-5" {
		t.Fatalf("model not stamped: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "stream_options.include_usage").Bool() != true {
		t.Fatalf("family-wide streaming usage injection missing: %s", res.Body)
	}
}

func TestGLMRewriteNative_NonStreamVerbatimWhenConformant(t *testing.T) {
	body := []byte(`{"model":"glm-5","messages":[]}`)
	res, err := New().RewriteNative(typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "glm-5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Body, body) {
		t.Fatalf("conformant body must pass through: %s", res.Body)
	}
}
