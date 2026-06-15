package perplexity

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/adaptertest"
)

// perplexity is a thin OpenAI-compat wrapper; its full behavior contract is
// verified by the shared adaptertest suite, parameterized with perplexity's
// identity fixtures. Note: perplexity serves chat on /chat/completions (no /v1).
func TestContract(t *testing.T) {
	adaptertest.RunContract(t, adaptertest.Case{
		Adapter:     &Adapter{},
		AdapterID:   adapterID,
		Provider:    "perplexity",
		KeyClass:    "perplexity-bearer",
		Host:        "api.perplexity.ai",
		ChatPath:    "/chat/completions",
		SampleModel: "sonar-pro",
		SampleToken: "pplx-abcdef123456",
	})
}
