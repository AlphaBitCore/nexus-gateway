package proxy

// hook_extraction_multimodal.go — gateway-local text-leaf extraction for the
// multimodal JSON routes (image generation, TTS).
//
// The shared traffic adapters deliberately do NOT extract these shapes: the
// adapters also run on the Compliance-Proxy / Agent interception paths, and
// extending shared extraction is gated on an NE fail-open review. This
// extractor is AI-Gateway-only: it reads the user-intent text slots straight
// off the ingress JSON body and feeds them to the hook pipeline as ordinary
// text ContentBlocks, so the rule-pack engine scans multimodal prompts with
// zero hook-layer changes.
//
// Coverage contract: multimodalUserTextPaths is the authoritative list of
// user-intent text slots per endpoint kind. Every user-text-bearing field of
// a supported wire shape MUST be listed here — an un-listed slot would skip
// both real-time scanning and redaction. The conformance test
// (TestMultimodalUserTextCoverage) locks the known OpenAI wire schemas
// field-by-field against this table; registering a new multimodal route
// without extending the table fails that gate.

import (
	"time"

	"github.com/tidwall/gjson"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// extractMultimodalForHooks runs the gateway-local multimodal extraction with
// the phase timing + traffic-extract metric the shared-adapter path also
// records. Split out of stage_hooks.go to keep that file at its size cap.
func (h *Handler) extractMultimodalForHooks(kind typology.EndpointKind, ingressFormat string, body []byte, pt *traffic.PhaseTimer) *normcore.NormalizedPayload {
	extractStart := time.Now()
	normalized := extractMultimodalRequestText(kind, body)
	if pt != nil {
		pt.MarkBetween(traffic.PhaseHookExtract, time.Since(extractStart))
	}
	if h.deps.Metrics != nil {
		outcome := "success"
		if normalized == nil {
			outcome = "skipped"
		}
		h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", outcome)
	}
	return normalized
}

// multimodalUserTextPaths returns the gjson paths of every user-intent text
// slot for a multimodal endpoint kind, or nil when the kind is not a
// gateway-locally-extracted multimodal endpoint.
//
//   - image_generation (/v1/images/generations): `prompt` — the only
//     user-text field on the OpenAI images wire; size/quality/n/response_format
//     are non-text knobs, `model` is routing metadata.
//   - tts (/v1/audio/speech): `input` — the synthesized text; voice/format/
//     speed are non-text knobs.
//
// STT is absent by design: its transcriptions route is not registered yet
// (multipart), and its user content is the audio artifact + the response
// transcript, neither of which is a request-side JSON text slot.
func multimodalUserTextPaths(kind typology.EndpointKind) []string {
	switch kind {
	case typology.EndpointKindImageGeneration:
		return []string{"prompt"}
	case typology.EndpointKindTTS:
		return []string{"input"}
	}
	return nil
}

// extractMultimodalRequestText builds the request-stage hook payload for a
// multimodal JSON body. Returns nil when the body carries no user text
// (nothing to scan — the hook pipeline then treats content hooks as
// no-input, exactly like the chat prefilter path).
//
// The payload's text blocks are ordinary user-message ContentBlocks, so
// TextProjection() covers them and redaction spans address them as
// messages.0.content.<i> — identical to chat. The Kind is stamped
// KindAIImage for image requests (the honest, existing vocabulary); TTS keeps
// the synthetic-chat Kind from PayloadFromTextSegments — this gateway-local
// request-scan payload does not need the modality Kind (hook gating reads
// HookInput.EndpointType, not the payload Kind), so it is left as-is even though
// the shared vocabulary now has KindAITTS/KindAISTT for the view-time codecs.
func extractMultimodalRequestText(kind typology.EndpointKind, body []byte) *normcore.NormalizedPayload {
	paths := multimodalUserTextPaths(kind)
	if len(paths) == 0 || len(body) == 0 {
		return nil
	}
	segments := make([]string, 0, len(paths))
	for _, p := range paths {
		v := gjson.GetBytes(body, p)
		switch {
		case v.Type == gjson.String && v.Str != "":
			segments = append(segments, v.Str)
		case v.IsArray():
			// A client can pass the text slot as an array of strings; each
			// element is user text and MUST be scanned (a bare string-type
			// check would let `{"prompt":["pii…"]}` bypass the hook engine).
			for _, el := range v.Array() {
				if el.Type == gjson.String && el.Str != "" {
					segments = append(segments, el.Str)
				}
			}
		}
	}
	if len(segments) == 0 {
		return nil
	}
	payload := hookcore.PayloadFromTextSegments(segments)
	if kind == typology.EndpointKindImageGeneration {
		payload.Kind = normcore.KindAIImage
	}
	return payload
}

// multimodalCoverage returns the request-time compliance-coverage value for a
// multimodal endpoint, or "" for non-multimodal (chat/embeddings make no
// claim). Honesty is the whole point of the field, so "prompt-only" is
// returned ONLY when the prompt text was actually put in front of a
// content-scanning hook: the pipeline must contain a content hook (a
// metadata-only pipeline of rate-limit / IP / size / webhook hooks scans no
// content), AND the prompt must have been evaluated — either the raw-body
// prefilter superset scan ran (content scanned, found clean) or structured
// extraction produced text. A present-but-unscannable slot (non-string /
// non-string-array) yields no text → "none", never a false "scanned" claim.
// Emergency hook-bypass and a hooks-off pipeline also map to "none".
func multimodalCoverage(kind typology.EndpointKind, hasContentHook, scanned bool) string {
	if multimodalUserTextPaths(kind) == nil {
		return ""
	}
	if hasContentHook && scanned {
		return "prompt-only"
	}
	return "none"
}
