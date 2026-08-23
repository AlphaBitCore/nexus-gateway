package replicate

import (
	"net/http"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// Replicate answers 402 for a billing problem. It used to map to auth_failed as
// the closest match the taxonomy then offered, which told an operator to rotate
// a key that was never the problem.
func TestNormalize_PaymentRequiredIsQuotaNotAuth(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusPaymentRequired, http.Header{},
		[]byte(`{"detail":"Insufficient credit"}`))
	if pe.Code != provcore.CodeProviderQuotaExhausted {
		t.Errorf("code = %q, want %q", pe.Code, provcore.CodeProviderQuotaExhausted)
	}

	// A real credential failure must not drift into the new code.
	unauth := errorNormalizer{}.Normalize(http.StatusUnauthorized, http.Header{}, []byte(`{}`))
	if unauth.Code != provcore.CodeAuthFailed {
		t.Errorf("401 code = %q, want %q", unauth.Code, provcore.CodeAuthFailed)
	}
}
