package canonicalext

import (
	"strings"
	"testing"
)

// Whatever Get HONOURS, Strip must REMOVE. Stated as one invariant rather than
// as a list of spellings, because the list is the part that was wrong: the
// removal scanned for the literal bytes `"nexus"` while gjson resolves escapes
// when it matches a key, so `{"\u006eexus":{...}}` was acted on by the gateway
// AND forwarded to the provider. That is the leak this package exists to close,
// reachable by one escape, and it is worse than an ignored key — the caller's
// extension took effect and then rode out to the upstream, which is how Cohere
// answered 422 "unknown field: parameter 'nexus'" and DeepSeek and Moonshot
// quietly kept the metadata.
func TestStrip_RemovesEverySpellingGetHonours(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"the plain spelling the gateway itself writes", `{"nexus":{"ext":{"anthropic":{"topK":42}}},"model":"m"}`},
		{"one letter escaped", `{"\u006eexus":{"ext":{"anthropic":{"topK":42}}},"model":"m"}`},
		{"the last letter escaped", `{"nexu\u0073":{"ext":{"anthropic":{"topK":42}}},"model":"m"}`},
		{"every letter escaped", `{"\u006e\u0065\u0078\u0075\u0073":{"ext":{"anthropic":{"topK":42}}},"model":"m"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !Get([]byte(tc.body), "anthropic", "topK").Exists() {
				t.Skip("this spelling is not honoured, so there is nothing to leak")
			}
			out := Strip([]byte(tc.body))
			if Get(out, "anthropic", "topK").Exists() {
				t.Errorf("the gateway acts on this extension and then forwards it: %s", out)
			}
			if !strings.Contains(string(out), `"model":"m"`) {
				t.Errorf("the rest of the body did not survive the removal: %s", out)
			}
		})
	}
}
