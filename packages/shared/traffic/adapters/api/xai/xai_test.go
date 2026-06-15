package xai

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/adaptertest"
)

// xai is a thin OpenAI-compat wrapper; its full behavior contract is verified
// by the shared adaptertest suite, parameterized with xai's identity fixtures.
func TestContract(t *testing.T) {
	adaptertest.RunContract(t, adaptertest.Case{
		Adapter:     &Adapter{},
		AdapterID:   adapterID,
		Provider:    "xai",
		KeyClass:    "xai-bearer",
		Host:        "api.x.ai",
		ChatPath:    "/v1/chat/completions",
		SampleModel: "grok-2-latest",
		SampleToken: "xai-abcdef123456",
	})
}
