package bedrock

import (
	"net/http"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// AWS names this case itself and it is not a throttle — backing off does not
// raise a service quota. It fell through to the status default, which read it
// as a rate limit and kept retrying the one account that cannot serve.
func TestNormalize_ServiceQuotaExceededIsNotAThrottle(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusTooManyRequests, http.Header{},
		[]byte(`{"__type":"ServiceQuotaExceededException","message":"quota for this model reached"}`))
	if pe.Code != provcore.CodeProviderQuotaExhausted {
		t.Errorf("code = %q, want %q", pe.Code, provcore.CodeProviderQuotaExhausted)
	}

	// The genuine throttle keeps its meaning; retrying it is the right move.
	throttle := errorNormalizer{}.Normalize(http.StatusTooManyRequests, http.Header{},
		[]byte(`{"__type":"ThrottlingException","message":"Too many requests"}`))
	if throttle.Code != provcore.CodeRateLimited {
		t.Errorf("throttle code = %q, want %q", throttle.Code, provcore.CodeRateLimited)
	}
}
