package proxy

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// TestPreCallBillableUnits_RerankReservesSearchUnitsNotTokens pins the class fix:
// the quota pre-check reserves the endpoint's OWN billable unit. A rerank request
// is priced per search unit, so reserving its documents' thousands of "tokens"
// against the per-search rate (2000 USD / 1M search units) would over-reserve by
// orders of magnitude and block the request under a cost quota. This is why the
// rerank price columns were kept NULL before the unified pricing semantic; with a
// real price, the pre-check MUST switch to search units.
func TestPreCallBillableUnits_RerankReservesSearchUnitsNotTokens(t *testing.T) {
	// 3 documents → 1 search unit (a search = 1 query + up to 100 documents).
	body := []byte(`{"model":"rerank-v3.5","query":"q","documents":["alpha","beta","gamma"]}`)
	in, out := preCallBillableUnits("rerank", body)
	if in != 1 || out != 0 {
		t.Errorf("rerank 3 docs → (input=%d, output=%d), want (1 search unit, 0)", in, out)
	}
	// The red-proof: tokens for the same body are many more than 1 unit, so a
	// token reservation would price this request far above its true per-search
	// cost. If the rerank branch were removed and this fell through to the token
	// default, `in` would equal estimateTokens(body) (> 1) and the assert above
	// would fail.
	if tok := estimateTokens(body); tok <= in {
		t.Errorf("token estimate %d is not larger than the %d search unit(s) — the divergence this fix exists to prevent is not demonstrated", tok, in)
	}

	// 250 documents → ceil(250/100) = 3 search units (Cohere counts >100 docs as
	// multiple searches). Over-counting is the safe direction for a cost cap.
	docs := make([]string, 250)
	for i := range docs {
		docs[i] = "d"
	}
	big, err := json.Marshal(map[string]any{"model": "rerank-v3.5", "documents": docs})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if in250, _ := preCallBillableUnits("rerank", big); in250 != 3 {
		t.Errorf("rerank 250 docs → %d search units, want 3 (ceil(250/100))", in250)
	}

	// Zero documents still reserves one search unit — never zero, which would
	// let the request bypass the cost cap entirely.
	if inZero, _ := preCallBillableUnits("rerank", []byte(`{"model":"rerank-v3.5","query":"q"}`)); inZero != 1 {
		t.Errorf("rerank 0 docs → %d, want 1 (minimum one search unit)", inZero)
	}
}

// TestPreCallBillableUnits_ImageReservesImagesNotPromptTokens pins the image
// half of the class fix. A per-image model (dall-e: inputPricePerMillion is USD
// per 1M images) priced by the prompt's bytes-as-tokens over-reserves by orders
// of magnitude — a ~200-byte prompt tokenizes to dozens of "images" and, under a
// cost quota, wrongly 429s a request that generates a single $0.04 image. The
// pre-check must reserve request.n images instead.
func TestPreCallBillableUnits_ImageReservesImagesNotPromptTokens(t *testing.T) {
	// n omitted → default 1 image.
	body := []byte(`{"model":"dall-e-3","prompt":"a photorealistic red bicycle on a beach at sunset"}`)
	in, out := preCallBillableUnits("image", body)
	if in != 1 || out != 0 {
		t.Errorf("image (no n) → (input=%d, output=%d), want (1 image, 0)", in, out)
	}
	// Red-proof: the token estimate for the same body is many more than 1, so a
	// token reservation would price this single-image request far above its true
	// per-image cost. If the image branch were removed and it fell through to the
	// token default, `in` would equal estimateTokens(body) (> 1).
	if tok := estimateTokens(body); tok <= in {
		t.Errorf("token estimate %d is not larger than the %d image(s) — the over-reservation this fix prevents is not demonstrated", tok, in)
	}
	// Explicit n=3 → reserve 3 images.
	if in3, out3 := preCallBillableUnits("image", []byte(`{"model":"dall-e-3","prompt":"x","n":3}`)); in3 != 3 || out3 != 0 {
		t.Errorf("image n=3 → (input=%d, output=%d), want (3, 0)", in3, out3)
	}
	// n=0 or absent floors at 1 — never zero, which would bypass the cost cap.
	if in0, _ := preCallBillableUnits("image", []byte(`{"model":"dall-e-3","prompt":"x","n":0}`)); in0 != 1 {
		t.Errorf("image n=0 → %d, want 1 (minimum one image)", in0)
	}
}

// TestPreCallBillableUnits_TTSReservesInputCharsNotBodyTokens pins the tts half.
// TTS is priced per input character; the post-call stamp counts runes of
// request.input, so the pre-check must reserve the SAME count — making the
// reservation reconcile exactly with the bill rather than approximating via the
// whole body's bytes/3.
func TestPreCallBillableUnits_TTSReservesInputCharsNotBodyTokens(t *testing.T) {
	input := "The quick brown fox jumps over the lazy dog." // 44 runes
	body := []byte(`{"model":"gpt-4o-mini-tts","voice":"alloy","input":"` + input + `"}`)
	in, out := preCallBillableUnits("tts", body)
	if want := int64(utf8.RuneCountInString(input)); in != want || out != 0 {
		t.Errorf("tts → (input=%d, output=%d), want (%d chars, 0)", in, out, want)
	}
	// The reserved count must equal what the stamp bills (runes of request.input),
	// not the token approximation of the whole JSON body.
	if tok := estimateTokens(body); tok == in {
		t.Errorf("tts reserved %d equals the token estimate %d — the char-vs-token divergence is not demonstrated", in, tok)
	}
	// Empty input floors at 1 so a request cannot price $0 and bypass the cap.
	if inEmpty, _ := preCallBillableUnits("tts", []byte(`{"model":"gpt-4o-mini-tts","input":""}`)); inEmpty != 1 {
		t.Errorf("tts empty input → %d, want 1 (minimum one character)", inEmpty)
	}
}

// TestPreCallBillableUnits_TokenEndpointsUnchanged confirms the fix is surgical:
// chat and every non-rerank type keep the token reservation (input tokens +
// max_tokens output), so the high-traffic token path is untouched.
func TestPreCallBillableUnits_TokenEndpointsUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello world"}],"max_tokens":500}`)
	in, out := preCallBillableUnits("chat", body)
	if in != estimateTokens(body) {
		t.Errorf("chat input units = %d, want estimateTokens = %d (token path unchanged)", in, estimateTokens(body))
	}
	if out != 500 {
		t.Errorf("chat output reservation = %d, want max_tokens 500", out)
	}
	// max_tokens omitted → the 4096 default reservation.
	if _, outDef := preCallBillableUnits("chat", []byte(`{"model":"gpt-4o","messages":[]}`)); outDef != 4096 {
		t.Errorf("chat with no max_tokens → output %d, want 4096 default", outDef)
	}
	// embedding (token-billed) also keeps the token path.
	if inEmb, _ := preCallBillableUnits("embedding", body); inEmb != estimateTokens(body) {
		t.Errorf("embedding input units = %d, want token estimate %d", inEmb, estimateTokens(body))
	}
}
