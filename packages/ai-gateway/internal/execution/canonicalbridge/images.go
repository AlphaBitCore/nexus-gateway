package canonicalbridge

// images.go — the image-kind lane of the bridge (cross-shape image
// codec). Canonical = the OpenAI images-generations shape; the only
// image ingress is OpenAI-shaped (/v1/images/generations), so
// "canonicalize" is validation + identity. The non-OpenAI leg opened in
// v1 is Gemini :generateContent with responseModalities:["IMAGE"]
// (Nano Banana models, which have no dedicated image endpoint); every
// additional non-OpenAI image leg is independently demand-gated and
// native passthrough stays the default for OpenAI-shape providers.

import (
	"fmt"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"net/http"
)

// imagesMaxN bounds the canonical `n` on the cross-format image lane
// before translation (billing-DoS guard: one request cannot multiply
// upstream image spend beyond the cap). The native OpenAI leg is not
// gated here — it keeps OpenAI's own documented bound, enforced
// upstream. The Gemini codec re-validates the same bound
// (defense-in-depth on the encode itself).
const imagesMaxN = 4

// ImagesRoutable reports whether image-generation traffic from ingress
// may route to a target. The only image ingress wire is OpenAI-shaped.
// v1 opens exactly two target legs, each proven end-to-end:
//   - FormatOpenAI — the literal native passthrough (P1, byte-unchanged);
//   - FormatGemini — the self-built cross-shape codec.
//
// Every OTHER format — including wire-adjacent OpenAI-family siblings
// (Azure, Moonshot, xAI, …) — is deliberately NOT routable: each is its
// own demand-gated leg needing real work before it can dispatch (the
// identity codec has no images encode case, the Azure transport has no
// images path, and admitting a family sibling here would also wrongly
// bind the Gemini-lane validation to it via the cross-format
// canonicalization gate). Routability that dispatch cannot serve is a
// dead leg that silently poisons failover — never advertise it.
func (b *Bridge) ImagesRoutable(ingress, target provcore.Format) bool {
	if !openAILike(ingress) {
		return false
	}
	switch target {
	case provcore.FormatOpenAI:
		return true
	case provcore.FormatGemini:
		_, ok := b.codecs[target]
		return ok
	}
	return false
}

// ImagesWireShapeForTarget returns the wire shape the image kind
// dispatches to for a target format: the OpenAI images wire for the
// literal OpenAI target (native passthrough), the image leg of Gemini
// :generateContent for Gemini. Formats with no opened image leg return
// WireShapeNone — ImagesRoutable gates them out of routing, so a None
// here reaching a codec is a programming error surfaced loudly by the
// codec's shape gate rather than silently mis-encoded.
func (b *Bridge) ImagesWireShapeForTarget(target provcore.Format) typology.WireShape {
	switch target {
	case provcore.FormatOpenAI:
		return typology.WireShapeOpenAIImages
	case provcore.FormatGemini:
		return typology.WireShapeGeminiImagesGenerateContent
	}
	return typology.WireShapeNone
}

// IngressImagesToCanonical converts a client ingress image-generation
// body to canonical OpenAI images JSON. The only image ingress is
// OpenAI-shaped, so this is validation + identity. It runs on the
// cross-format lane only (prepare stage for the primary target,
// IngressImagesToWire for failover targets); the native same-format
// passthrough is deliberately untouched (shipped P1 posture).
//
// Validation (each rejection names the field and the resolved
// provider/model — routing can rewrite the model mid-flight, so an
// anonymous 400 is a support ticket):
//   - `prompt` must be a non-empty string. The ingress scanner
//     defensively scans array-of-string prompts element-by-element; the
//     codec must never have to guess how to collapse one, so non-string
//     shapes are rejected and scan-shape == generate-shape holds.
//   - `n` must be an integer in [1, imagesMaxN].
//   - a top-level `nexus` key is rejected: the nexus.ext.* extension
//     channel does not exist on the image canonical (an open channel
//     would be a smuggling path into the closed field set).
func (b *Bridge) IngressImagesToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error) {
	if !openAILike(ingress) {
		return nil, fmt.Errorf("canonicalbridge: ingress format %q has no images canonical (the image ingress is OpenAI-shaped only)", ingress)
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("images body must be a JSON object")
	}
	resolved := fmt.Sprintf("%s (%s)", ct.Format, ct.ProviderModelID)
	if p := root.Get("prompt"); p.Type != gjson.String || p.Str == "" {
		return nil, fmt.Errorf("field %q must be a non-empty string for the resolved image provider %s", "prompt", resolved)
	}
	if n := root.Get("n"); n.Exists() {
		f := n.Float()
		v := int(f)
		if n.Type != gjson.Number || float64(v) != f || v < 1 || v > imagesMaxN {
			return nil, fmt.Errorf("field %q must be an integer in [1, %d] for the resolved image provider %s", "n", imagesMaxN, resolved)
		}
	}
	if root.Get("nexus").Exists() {
		return nil, fmt.Errorf("field %q is not supported on the images canonical for the resolved image provider %s", "nexus", resolved)
	}
	return body, nil
}

// IngressImagesToWire mirrors [Bridge.IngressChatToWire] for the image
// kind: canonical → target wire via the target codec. It internally
// invokes IngressImagesToCanonical FIRST, so the validation above binds
// on the executor failover lane too (matching IngressEmbeddingsToWire's
// structure) — without this, a request rejected on a Gemini primary
// could reach the Gemini wire unvalidated via failover.
//
// Unlike the chat counterpart it returns the codec's EncodeResult
// rewrites: the image codec records every coercion (size→aspectRatio,
// quality/style/user drops, response_format degradations) and the
// caller-visible x-nexus-coerced contract must survive failover legs,
// where the prepare stage's rewrite plumbing never ran.
func (b *Bridge) IngressImagesToWire(ingress, target provcore.Format, body []byte, ct provcore.CallTarget) (wireBody []byte, rewrites []string, err error) {
	if ingress == target {
		return body, nil, nil
	}
	canon, err := b.IngressImagesToCanonical(ingress, body, ct)
	if err != nil {
		return nil, nil, err
	}
	codec, ok := b.codecs[target]
	if !ok || codec == nil {
		return nil, nil, fmt.Errorf("canonicalbridge: no codec for format %q", target)
	}
	encRes, err := codec.EncodeRequest(b.ImagesWireShapeForTarget(target), canon, ct)
	if err != nil {
		return nil, nil, err
	}
	return encRes.Body, encRes.Rewrites, nil
}

// ValidateImagesIngressGuards applies the SPEND guard to an images body that
// stays on its own wire, so the same-format leg is bounded by the same rule as
// the cross-format one.
//
// The two ceilings on this endpoint answer different questions, and only one of
// them belongs to us. What a given upstream will accept is a wire fact the
// codec owns — Gemini refuses more than one candidate, and its codec says so
// with the probe that established it. How much spend one request may multiply
// is our decision, and it was applied on the cross-format lane only: a native
// OpenAI image request with n up to 10 reached the upstream ungated, and
// measurement confirms it — n=11 came back as OpenAI's own "Expected a value
// <= 10", which is the upstream refusing, not us. A billing guard that covers
// one of two legs is not a billing guard.
//
// Deliberately NOT enforced here: the canonical shape rules. Those exist so the
// cross-format codec has something it can translate; on a passthrough leg the
// provider's own opinion of its body is the one that counts, and rejecting a
// body it would have served is a regression rather than a guard. This mirrors
// the rerank passthrough guard, which draws the same line.
func (b *Bridge) ValidateImagesIngressGuards(ingress provcore.Format, body []byte, ct provcore.CallTarget) error {
	if !openAILike(ingress) {
		return fmt.Errorf("canonicalbridge: ingress format %q has no images canonical (the image ingress is OpenAI-shaped only)", ingress)
	}
	n := gjson.GetBytes(body, "n")
	if !n.Exists() {
		return nil
	}
	f := n.Float()
	v := int(f)
	if n.Type != gjson.Number || float64(v) != f || v < 1 || v > imagesMaxN {
		// Typed, so writeCodecErr keeps the code instead of flattening this to
		// CODEC_ENCODE_FAILED with a "canonicalize ingress body:" prefix — on a
		// leg where no canonicalization happens. Nothing failed to encode; a
		// ceiling was enforced, and the row's error_code should say so.
		return &provcore.ProviderError{
			Status: http.StatusBadRequest,
			Code:   provcore.CodeSpendLimitExceeded,
			Message: fmt.Sprintf("field %q must be an integer in [1, %d] for the resolved image provider %s (%s)",
				"n", imagesMaxN, ct.Format, ct.ProviderModelID),
		}
	}
	return nil
}
