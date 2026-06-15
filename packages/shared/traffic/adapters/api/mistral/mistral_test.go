package mistral

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/adaptertest"
)

// mistral is a thin OpenAI-compat wrapper; its full behavior contract is
// verified by the shared adaptertest suite, parameterized with mistral's
// identity fixtures.
func TestContract(t *testing.T) {
	adaptertest.RunContract(t, adaptertest.Case{
		Adapter:     &Adapter{},
		AdapterID:   adapterID,
		Provider:    "mistral",
		KeyClass:    "mistral-bearer",
		Host:        "api.mistral.ai",
		ChatPath:    "/v1/chat/completions",
		SampleModel: "mistral-large-latest",
		SampleToken: "mistral_api_key_abc",
	})
}
