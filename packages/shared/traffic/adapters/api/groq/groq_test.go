package groq

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/adaptertest"
)

// groq is a thin OpenAI-compat wrapper; its full behavior contract is verified
// by the shared adaptertest suite, parameterized with groq's identity fixtures.
func TestContract(t *testing.T) {
	adaptertest.RunContract(t, adaptertest.Case{
		Adapter:     &Adapter{},
		AdapterID:   adapterID,
		Provider:    "groq",
		KeyClass:    "groq-bearer",
		Host:        "api.groq.com",
		ChatPath:    "/openai/v1/chat/completions",
		SampleModel: "llama-3.3-70b-versatile",
		SampleToken: "gsk_abcdef123456",
	})
}
