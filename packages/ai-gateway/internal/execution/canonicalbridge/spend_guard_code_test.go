package canonicalbridge

import (
	"errors"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"strings"
)

// A spend refusal has to say it is a spend refusal.
//
// Both passthrough guards returned a bare error, so writeCodecErr fell through
// to its untyped branch: the caller saw code CODEC_ENCODE_FAILED and a message
// prefixed "canonicalize ingress body:" — on a leg where no canonicalization
// happens at all. Measured on production:
//
//	POST /v1/images/generations {"n":5}
//	→ {"code":"CODEC_ENCODE_FAILED",
//	   "message":"canonicalize ingress body: field \"n\" must be an integer …"}
//
// Nothing failed to encode. A bound was enforced deliberately. The consequence
// is not cosmetic: the traffic row's error_code says the same, so an operator
// grouping by code files spend refusals under a codec bug, and a caller cannot
// tell "I asked for too many images" from "the gateway could not translate my
// body".
func TestSpendGuards_CarryTheirOwnCode(t *testing.T) {
	b := New(nil)

	t.Run("images n above the ceiling", func(t *testing.T) {
		err := b.ValidateImagesIngressGuards(provcore.FormatOpenAI,
			[]byte(`{"prompt":"x","n":5}`),
			provcore.CallTarget{Format: provcore.FormatOpenAI, ProviderModelID: "gpt-image-1-mini"})
		assertTypedCode(t, err, provcore.CodeSpendLimitExceeded)
	})

	t.Run("rerank documents above the ceiling", func(t *testing.T) {
		docs := make([]byte, 0, 1<<16)
		docs = append(docs, `{"model":"m","query":"q","documents":[`...)
		for i := range rerankMaxDocuments + 1 {
			if i > 0 {
				docs = append(docs, ',')
			}
			docs = append(docs, `"d"`...)
		}
		docs = append(docs, `]}`...)
		err := b.ValidateRerankIngressGuards(provcore.FormatCohere, docs,
			provcore.CallTarget{Format: provcore.FormatCohere, ProviderModelID: "rerank-v3.5"})
		assertTypedCode(t, err, provcore.CodeSpendLimitExceeded)
	})

	// A genuinely malformed body is NOT a spend refusal and must not borrow the
	// code — that would put the caller's mistake in the operator's spend bucket.
	t.Run("a malformed body keeps the generic path", func(t *testing.T) {
		err := b.ValidateRerankIngressGuards(provcore.FormatCohere,
			[]byte(`{"model":"m","query":"q","documents":"not-an-array"}`),
			provcore.CallTarget{Format: provcore.FormatCohere, ProviderModelID: "rerank-v3.5"})
		if err == nil {
			t.Fatal("a non-array documents field was accepted")
		}
		var pe *provcore.ProviderError
		if errors.As(err, &pe) && pe.Code == provcore.CodeSpendLimitExceeded {
			t.Errorf("a malformed body was filed as a spend refusal: %v", err)
		}
	})
}

func assertTypedCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("the ceiling was not enforced at all")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error is untyped (%T: %v) — writeCodecErr will file it as CODEC_ENCODE_FAILED", err, err)
	}
	if pe.Code != want {
		t.Errorf("code = %q, want %q", pe.Code, want)
	}
	// The gateway's own error surface is UPPER_SNAKE by a contract sdk_compat
	// pins; this code reaches a caller through that surface, not through a
	// normalised provider error, so it follows that convention and not the
	// lower_snake one its neighbours in providers/core use.
	if pe.Code != strings.ToUpper(pe.Code) {
		t.Errorf("code %q is not UPPER_SNAKE — it ships on the gateway envelope", pe.Code)
	}
	if pe.Status != 400 {
		t.Errorf("status = %d, want 400", pe.Status)
	}
}
