package together

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/adaptertest"
)

// together is a thin OpenAI-compat wrapper; its full behavior contract is
// verified by the shared adaptertest suite, parameterized with together's
// identity fixtures.
func TestContract(t *testing.T) {
	adaptertest.RunContract(t, adaptertest.Case{
		Adapter:     &Adapter{},
		AdapterID:   adapterID,
		Provider:    "together",
		KeyClass:    "together-bearer",
		Host:        "api.together.xyz",
		ChatPath:    "/v1/chat/completions",
		SampleModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo",
		SampleToken: "together_api_key_abc",
	})
}
