package codec

// images.go — the cross-shape image leg: canonical OpenAI images
// (/v1/images/generations) ↔ Gemini :generateContent with
// responseModalities:["IMAGE"] (Nano Banana models, which have no
// dedicated image endpoint).
//
// The encode side is ALLOW-LIST-ONLY: the OpenAI images canonical is a
// closed field set, and any top-level key outside the disposition table
// below is rejected with a 400 naming the field and the resolved
// provider/model — never forwarded. This deliberately departs from the
// chat codec's warn-and-forward posture: forwarding unknown fields to
// :generateContent would let a caller enable tool-augmented generation
// (tools/toolConfig), relax provider safetySettings, or smuggle intent
// text into systemInstruction that the prompt scanner (which reads only
// the `prompt` leaf) never sees.
//
// The canonical body is parsed with gjson (first-occurrence semantics),
// the same parser family the ingress scanner uses, so a duplicate-key
// body cannot be scanned as one value and generated from another.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// geminiImageMaxN bounds OpenAI `n` on this leg. Observed 2026-07-15 on
// gemini-2.5-flash-image via the live leg:
//
//	400 "Multiple candidates is not enabled for this model"
//
// for generationConfig.candidateCount=2 — so the wire quirk narrows the
// bridge's canonical fan-out bound ([1,4]) to exactly 1 here, and n=1
// omits candidateCount entirely (the upstream default; one fewer
// rejectable field). The native OpenAI leg keeps OpenAI's own
// documented bound (n ≤ 10, enforced upstream).
const geminiImageMaxN = 1

// geminiImageAspectRatios is the closed lossy map from the documented
// OpenAI image sizes to Gemini imageConfig.aspectRatio values. Pixel
// dimensions cannot survive an aspect-ratio coercion (image is carved
// out of the round-trip-identity standard); every hit records a rewrite
// so the coercion is caller-visible on x-nexus-coerced. Values outside
// this map are rejected 400 — never a silent nearest-fit. The domain is
// exactly the documented OpenAI surface (dall-e-2 squares, dall-e-3,
// gpt-image-1); the range is documented Gemini ratios.
var geminiImageAspectRatios = map[string]string{
	"256x256":   "1:1",
	"512x512":   "1:1",
	"1024x1024": "1:1",
	"1792x1024": "16:9",
	"1024x1792": "9:16",
	"1536x1024": "3:2",
	"1024x1536": "2:3",
}

// errImageField returns the closed-set rejection for a canonical image
// field this leg cannot honor. The message names the field AND the
// resolved provider/model: routing rules and quota downgrades can
// rewrite the model mid-flight, so a 400 that does not say which
// provider was resolved is a support ticket. The provider name derives
// from the resolved target (same composition as the bridge's
// validation errors — one vocabulary across both layers, and it stays
// correct if a wire-identical sibling leg reuses this codec).
func errImageField(field string, target provcore.CallTarget, reason string) error {
	return &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeInvalidRequest,
		Type:    "nexus_field_unsupported",
		Message: fmt.Sprintf("nexus: field %q %s for the resolved image provider %s (%s)", field, reason, target.Format, target.ProviderModelID),
	}
}

// encodeGeminiImagesRequest translates a canonical OpenAI images body
// into the Gemini :generateContent image wire. Every top-level key is
// mapped, coerced-with-a-recorded-rewrite, or rejected 400 — there is
// no default arm that forwards an unrecognized key (R-8).
//
// Rewrite markers are gateway-composed constants (value-free for
// caller-supplied string fields): they ride a response header, so
// caller bytes never reach header framing.
func encodeGeminiImagesRequest(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("gemini images: empty canonical body")
	}
	root := gjson.ParseBytes(canonicalBody)
	if !root.IsObject() {
		return provcore.EncodeResult{}, fmt.Errorf("gemini images: canonical body is not a JSON object")
	}
	genCfg := map[string]any{
		// Codec-controlled, not caller input: this is what makes
		// :generateContent an image call.
		"responseModalities": []string{"IMAGE"},
	}
	var prompt string
	var rewrites []string
	responseFormatSeen := false

	var fieldErr error
	// First-occurrence-wins on duplicate top-level keys: gjson's Get —
	// which the ingress scanner uses to read the prompt — returns the
	// FIRST occurrence, while ForEach visits every occurrence. Without
	// this guard a duplicate-`prompt` body would be scanned as the first
	// value but generated from the last (the R-5 parser divergence this
	// codec is pinned against).
	seen := make(map[string]bool, 8)
	root.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		if seen[k] {
			return true
		}
		seen[k] = true
		switch k {
		case "model":
			// Resolved upstream model rides the URL path
			// (Transport.BuildURL from ProviderModelID), never the body.
		case "prompt":
			if value.Type != gjson.String || value.Str == "" {
				// Non-string prompts are rejected so the wire can never
				// carry a differently-collapsed shape of what the scanner
				// scanned (the scanner defensively scans array prompts
				// element-by-element; the codec must not guess a join).
				fieldErr = errImageField("prompt", target, "must be a non-empty string")
				return false
			}
			prompt = value.Str
		case "n":
			f := value.Float()
			n := int(f)
			if value.Type != gjson.Number || float64(n) != f || n < 1 || n > geminiImageMaxN {
				fieldErr = errImageField("n", target, fmt.Sprintf("must be %d (the upstream rejects multiple image candidates)", geminiImageMaxN))
				return false
			}
			// n == 1 is the upstream default; candidateCount is omitted
			// (see the geminiImageMaxN observation).
		case "size":
			s := value.String()
			if s == "" || s == "auto" {
				// Provider default; nothing to coerce.
				return true
			}
			ratio, ok := geminiImageAspectRatios[s]
			if !ok {
				fieldErr = errImageField("size", target, "is not a documented OpenAI image size")
				return false
			}
			genCfg["imageConfig"] = map[string]any{"aspectRatio": ratio}
			rewrites = append(rewrites, "size:"+s+"→aspectRatio "+ratio)
		case "quality":
			rewrites = append(rewrites, "quality→dropped")
		case "style":
			rewrites = append(rewrites, "style→dropped")
		case "user":
			rewrites = append(rewrites, "user→dropped")
		case "response_format":
			responseFormatSeen = true
			switch value.String() {
			case "b64_json":
				// Native: Gemini returns inline base64.
			case "url":
				// The gateway cannot mint artifact URLs before the
				// artifact store + its access-control surface ship; the
				// caller still receives the image bytes, inline, and the
				// coercion is explicit — never a silent substitution.
				rewrites = append(rewrites, "response_format:url→b64_json")
			default:
				fieldErr = errImageField("response_format", target, `must be "b64_json" or "url"`)
				return false
			}
		case "stream":
			// Admission forces multimodal non-stream before dispatch;
			// `stream` is not a Gemini field and is not forwarded. This is
			// the one deliberately unmarked disposition (shipped P1
			// posture, target-independent).
		default:
			fieldErr = errImageField(k, target, "is not supported")
			return false
		}
		return true
	})
	if fieldErr != nil {
		return provcore.EncodeResult{}, fieldErr
	}
	if prompt == "" {
		return provcore.EncodeResult{}, errImageField("prompt", target, "must be a non-empty string")
	}
	if !responseFormatSeen {
		// OpenAI's documented default for dall-e models is `url`; this leg
		// returns inline base64, so the default-path divergence is recorded
		// too — never silent.
		rewrites = append(rewrites, "response_format:default→b64_json")
	}

	// Structured build: caller text is only ever a JSON string value, so
	// a crafted prompt cannot escape into sibling keys.
	wire := map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": prompt}},
		}},
		"generationConfig": genCfg,
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return provcore.EncodeResult{}, fmt.Errorf("gemini images: marshal wire body: %w", err)
	}
	return provcore.EncodeResult{Body: body, Rewrites: rewrites}, nil
}

// errImageContentPolicy is the fail-closed decode result for a
// provider-safety-blocked generation. Code MUST stay CodeInvalidRequest:
// the executor's failover classifier keys on the Code and treats it as
// terminal (no retry, no failover) — a safety-blocked prompt is never
// re-dispatched to the next paid target. Content-policy semantics ride
// the Type/message.
func errImageContentPolicy(reason string) error {
	return &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeInvalidRequest,
		Type:    "content_policy_violation",
		Message: "gemini: image generation blocked by provider safety: " + reason,
	}
}

// decodeGeminiImagesResponse translates a Gemini :generateContent image
// response into the canonical OpenAI images response shape. Ingress ==
// canonical for images, so the output is returned to the caller as-is.
//
// Fail-closed: this never produces a 200 with an empty data[] — a
// prompt-feedback/safety block surfaces as a content-policy 400
// (structured *ProviderError, propagated verbatim by the dispatcher),
// and a response that carries no image at all surfaces as a decode
// error (502-class, failover-eligible as usual).
func decodeGeminiImagesResponse(nativeBody []byte) (provcore.DecodeResult, error) {
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{}, fmt.Errorf("gemini images: empty upstream body")
	}
	root := gjson.ParseBytes(nativeBody)
	usage := provcore.ExtractUsage(nativeBody, provcore.FormatGemini)

	candidates := root.Get("candidates")
	if !candidates.IsArray() || len(candidates.Array()) == 0 {
		if reason := root.Get("promptFeedback.blockReason").String(); reason != "" {
			return provcore.DecodeResult{}, errImageContentPolicy(reason)
		}
		return provcore.DecodeResult{}, fmt.Errorf("gemini images: response carries no candidates")
	}

	type imageDatum struct {
		B64JSON string `json:"b64_json"`
	}
	var data []imageDatum
	safetyStopped := false
	candidates.ForEach(func(_, cand gjson.Result) bool {
		switch cand.Get("finishReason").String() {
		case "SAFETY", "IMAGE_SAFETY", "PROHIBITED_CONTENT":
			safetyStopped = true
		}
		cand.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			// Text parts are dropped: the OpenAI images response has no
			// text slot (a deliberate, documented loss — output-side
			// content is outside the compliance surface).
			if b64 := part.Get("inlineData.data"); b64.Type == gjson.String && b64.Str != "" {
				data = append(data, imageDatum{B64JSON: b64.Str})
			}
			return true
		})
		return true
	})

	if len(data) == 0 {
		if safetyStopped {
			return provcore.DecodeResult{}, errImageContentPolicy("candidate stopped by safety filter")
		}
		// The upstream answered without an image (e.g. text-only): a
		// fabricated-empty success would corrupt cost (0 images) and
		// caller logic — fail the decode instead (502 upstream-shape).
		return provcore.DecodeResult{}, fmt.Errorf("gemini images: response carries no image parts")
	}

	canon, err := json.Marshal(struct {
		Created int64        `json:"created"`
		Data    []imageDatum `json:"data"`
	}{
		Created: time.Now().Unix(),
		Data:    data,
	})
	if err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("gemini images: marshal canonical response: %w", err)
	}
	return provcore.DecodeResult{CanonicalBody: canon, Usage: usage}, nil
}
