package fireworks

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/adaptertest"
)

// fireworks is a thin OpenAI-compat wrapper; its full behavior contract is
// verified by the shared adaptertest suite, parameterized with fireworks'
// identity fixtures.
func TestContract(t *testing.T) {
	adaptertest.RunContract(t, adaptertest.Case{
		Adapter:     &Adapter{},
		AdapterID:   adapterID,
		Provider:    "fireworks",
		KeyClass:    "fireworks-bearer",
		Host:        "api.fireworks.ai",
		ChatPath:    "/v1/chat/completions",
		SampleModel: "accounts/fireworks/models/llama-v3p3-70b-instruct",
		SampleToken: "fw_xxx",
	})
}
