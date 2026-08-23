package voyage

import (
	"net/http"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// No documented signal here, so the conservative marker check on the
// invalid_request arm only. The marker set deliberately excludes the bare
// phrase "quota exceeded", which is also how per-minute rate limits are
// worded — an ambiguous message must stay invalid_request rather than be
// guessed into a failover.
func TestNormalize_QuotaMarkerOnTheInvalidRequestArm(t *testing.T) {
	spent := errorNormalizer{}.Normalize(http.StatusBadRequest, http.Header{},
		[]byte(`{"message":"your credit balance is too low to access this service"}`))
	if spent.Code != provcore.CodeProviderQuotaExhausted {
		t.Errorf("code = %q, want %q", spent.Code, provcore.CodeProviderQuotaExhausted)
	}

	plain := errorNormalizer{}.Normalize(http.StatusBadRequest, http.Header{},
		[]byte(`{"message":"invalid request: documents must be a list"}`))
	if plain.Code != provcore.CodeInvalidRequest {
		t.Errorf("plain 400 code = %q, want %q", plain.Code, provcore.CodeInvalidRequest)
	}

	// A 429 is left alone: without a discriminator, moving a throttled request
	// off the provider is worse than waiting a second.
	throttle := errorNormalizer{}.Normalize(http.StatusTooManyRequests, http.Header{},
		[]byte(`{"message":"quota exceeded"}`))
	if throttle.Code != provcore.CodeRateLimited {
		t.Errorf("429 code = %q, want %q", throttle.Code, provcore.CodeRateLimited)
	}
}
