package proxy

import "testing"

// A missing price row means "an operator forgot" for every model billed per
// token, and the quota check fails closed so unaccounted spend cannot slip
// past a cost cap. For rerank it means the opposite: Cohere bills per search
// unit, and a standing catalog assertion deliberately keeps the per-token
// columns NULL rather than carry a fabricated number or a zero claiming the
// endpoint is free. Failing closed there made rerank return 503 to every
// caller holding an application virtual key — the key type real customers
// hold — while personal and service keys skipped the cost check entirely,
// which is why the smoke suite never saw it.
func TestTokenBilled_RerankIsTheOnlyExemption(t *testing.T) {
	if tokenBilled("rerank") {
		t.Error("rerank is billed per search unit; a NULL per-token price is its normal state, not a misconfiguration")
	}
	// Everything else must keep failing closed. image, tts and video carry
	// per-token approximations in the catalog today, so a NULL there really is
	// someone forgetting — turning this into a family exemption would let that
	// spend go uncounted.
	for _, mt := range []string{"chat", "embedding", "image", "tts", "stt", "video", "realtime", ""} {
		if !tokenBilled(mt) {
			t.Errorf("model type %q must still fail closed on a missing price row", mt)
		}
	}
}
