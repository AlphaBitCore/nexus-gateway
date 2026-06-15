package moonshot

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/adaptertest"
)

// moonshot is a thin OpenAI-compat wrapper; its full behavior contract is
// verified by the shared adaptertest suite, parameterized with moonshot's
// identity fixtures.
func TestContract(t *testing.T) {
	adaptertest.RunContract(t, adaptertest.Case{
		Adapter:     &Adapter{},
		AdapterID:   adapterID,
		Provider:    "moonshot",
		KeyClass:    "moonshot-bearer",
		Host:        "api.moonshot.ai",
		ChatPath:    "/v1/chat/completions",
		SampleModel: "moonshot-v1-8k",
		SampleToken: "sk-moonshot-xyz",
	})
}
