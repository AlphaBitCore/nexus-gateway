package errors

import (
	"net/http"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// A spent account budget and a rate limit ride the same 429 and want opposite
// handling: backing off clears a rate limit and does nothing at all for an
// account with no money in it. Classified as rate-limited, the executor kept
// retrying the one provider that cannot serve the request instead of moving to
// one that can. OpenAI publishes a code for this, so it is read structurally
// rather than guessed from the message.
//
// This normalizer also serves Azure and GLM, which wire ErrorNormalizerInstance().
func TestNormalize_QuotaExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"documented code on a 429", 429, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`, provcore.CodeProviderQuotaExhausted},
		{"a real rate limit stays a rate limit", 429, `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-4o"}}`, provcore.CodeRateLimited},
		{"a 429 with no code stays a rate limit", 429, `{"error":{"message":"slow down"}}`, provcore.CodeRateLimited},
		{"compat upstream phrases it on a 400", 400, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details"}}`, provcore.CodeProviderQuotaExhausted},
		{"an ordinary 400 is still the caller's fault", 400, `{"error":{"message":"Invalid value for 'temperature'"}}`, provcore.CodeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := ErrorNormalizer{}.Normalize(tc.status, http.Header{}, []byte(tc.body))
			if pe.Code != tc.want {
				t.Errorf("code = %q, want %q", pe.Code, tc.want)
			}
		})
	}
}
