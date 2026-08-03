package proxy

// modality_stamps.go — the two multimodal cost/audit stamps computed at the
// non-stream response site (the only path multimodal traffic takes: both
// live routes are non-stream and endpoint-skip the cache):
//
//   - stampModalityUnits fills the modality BillableUnits so the per-kind
//     cost formulas price real consumption. Without this the image/TTS
//     formulas would receive zero units and price every request at $0.
//   - buildArtifactRefs computes the non-PII artifact reference JSON
//     persisted to traffic_event.artifact_refs. Byte-bearing artifacts
//     (inline b64_json images, TTS audio bodies) get a sha256 fingerprint
//     computed over bytes the gateway already holds in the buffered
//     non-stream response — no extra buffering, no streaming-egress
//     regression. URL-return images store the URL reference ONLY: the
//     gateway never dereferences it (SSRF guard), and the capability
//     matrix honestly reports no content hash exists in that mode.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// ArtifactRef is one artifact reference persisted (JSON-encoded, in an
// array) to traffic_event.artifact_refs. Exactly one of the two forms is
// populated: byte-bearing (Sha256+SizeBytes+Mime) or URL-return (URL only).
type ArtifactRef struct {
	Sha256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Mime      string `json:"mime,omitempty"`
	URL       string `json:"url,omitempty"`
}

// stampModalityUnits fills the modality unit fields consumed by the
// per-kind cost formulas (estimator.imageCostFormula etc.). Token fields
// are stamped by the caller from provider usage and always win inside the
// formulas; these units are the fallback for per-unit-priced models.
//
//   - image_generation: generated image count = len(response data[]) — the
//     actual output, not the requested n.
//   - tts: synthesized character count of the FORWARDED request `input`
//     (the redacted body when redaction applied — which is exactly what
//     the upstream billed).
//
// No-op for non-multimodal kinds and for absent bodies.
func stampModalityUnits(units *estimator.BillableUnits, endpointKind string, requestBody, responseBody []byte) {
	switch endpointKind {
	case string(typology.EndpointKindImageGeneration):
		if n := gjson.GetBytes(responseBody, "data.#").Int(); n > 0 {
			units.Images = int(n)
		}
	case string(typology.EndpointKindTTS):
		if in := gjson.GetBytes(requestBody, "input"); in.Type == gjson.String {
			units.InputChars = utf8.RuneCountInString(in.Str)
		}
	case string(typology.EndpointKindRerank):
		// Cohere reports the authoritative billable unit in the canonical
		// response (meta.billed_units.search_units). Voyage rerank prices by
		// tokens instead — those flow through the token path via PromptTokens,
		// so a missing search_units here is expected for Voyage, not an error.
		if su := gjson.GetBytes(responseBody, "meta.billed_units.search_units"); su.Exists() {
			units.SearchUnits = int(su.Int())
		}
	}
}

// buildArtifactRefs returns the JSON-encoded artifact reference array for a
// successful multimodal response, or "" when the kind produces no artifact
// or nothing is extractable.
//
//   - tts: the whole response body IS the artifact (binary audio) —
//     fingerprint it as held.
//   - image_generation: one ref per data[] element. b64_json → decode +
//     fingerprint the artifact bytes; url → reference only, never fetched.
func buildArtifactRefs(endpointKind string, responseBody []byte, contentType string) string {
	var refs []ArtifactRef
	switch endpointKind {
	case string(typology.EndpointKindTTS):
		if len(responseBody) == 0 {
			return ""
		}
		sum := sha256.Sum256(responseBody)
		refs = []ArtifactRef{{
			Sha256:    hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(responseBody)),
			Mime:      contentType,
		}}
	case string(typology.EndpointKindImageGeneration):
		data := gjson.GetBytes(responseBody, "data")
		if !data.IsArray() {
			return ""
		}
		for _, item := range data.Array() {
			if b64 := item.Get("b64_json"); b64.Type == gjson.String && b64.Str != "" {
				raw, err := base64.StdEncoding.DecodeString(b64.Str)
				if err != nil {
					continue // malformed artifact: skip the ref, never fail the request
				}
				sum := sha256.Sum256(raw)
				refs = append(refs, ArtifactRef{
					Sha256:    hex.EncodeToString(sum[:]),
					SizeBytes: int64(len(raw)),
					// Sniff the real image type from the decoded bytes:
					// gpt-image-1 returns webp/jpeg, dall-e PNG — a hardcoded
					// image/png would put a wrong mime on the audit artifact.
					Mime: sniffImageMime(raw),
				})
				continue
			}
			if u := item.Get("url"); u.Type == gjson.String && u.Str != "" {
				// URL-return mode: reference only. NEVER dereferenced
				// server-side; no content hash exists in this mode.
				refs = append(refs, ArtifactRef{URL: u.Str})
			}
		}
	default:
		return ""
	}
	if len(refs) == 0 {
		return ""
	}
	out, err := json.Marshal(refs)
	if err != nil {
		return ""
	}
	return string(out)
}

// sniffImageMime returns the MIME type of a decoded image artifact from its
// magic bytes, falling back to a generic octet-stream (never a wrong-but-
// specific guess). Covers the formats OpenAI image endpoints return.
func sniffImageMime(b []byte) string {
	switch {
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 12 && b[0] == 'R' && b[1] == 'I' && b[2] == 'F' && b[3] == 'F' &&
		b[8] == 'W' && b[9] == 'E' && b[10] == 'B' && b[11] == 'P':
		return "image/webp"
	case len(b) >= 6 && b[0] == 'G' && b[1] == 'I' && b[2] == 'F' && b[3] == '8':
		return "image/gif"
	default:
		// Fall back to Go's content sniffer, then octet-stream — never
		// assume a specific format we did not detect.
		if ct := http.DetectContentType(b); ct != "application/octet-stream" {
			return ct
		}
		return "application/octet-stream"
	}
}

// warnUnderivableModalityUnits reports whether a multimodal 2xx response
// produced no billable units at all (neither usage tokens nor a modality
// unit), i.e. it will price at $0 — the case the stamping site must surface.
func warnUnderivableModalityUnits(endpointKind string, statusCode int, u estimator.BillableUnits) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return false // token-priced path derived units
	}
	switch endpointKind {
	case string(typology.EndpointKindImageGeneration):
		return u.Images == 0
	case string(typology.EndpointKindTTS):
		return u.InputChars == 0
	case string(typology.EndpointKindSTT):
		return u.AudioSeconds == 0
	case string(typology.EndpointKindRerank):
		// $0 only when neither the token path (Voyage) nor search units
		// (Cohere) produced a unit — a genuine underivable-cost case (R-5).
		return u.SearchUnits == 0
	}
	return false
}

// underivableWarned dedups the underivable-units WARN to one line per
// endpoint kind for the process lifetime (mirrors the estimator's
// unregisteredWarned), so a burst of malformed/truncated responses does not
// flood the log while still surfacing the gap exactly once.
var underivableWarned sync.Map // endpoint string -> struct{}

func warnUnderivableUnitsOnce(logger *slog.Logger, endpointKind, modelID string, truncated bool) {
	if logger == nil {
		return
	}
	if _, loaded := underivableWarned.LoadOrStore(endpointKind, struct{}{}); loaded {
		return
	}
	logger.Warn("multimodal response yielded no billable units; request priced at $0 (truncated or malformed response, or missing input field)",
		slog.String("endpoint", endpointKind),
		slog.String("model", modelID),
		slog.Bool("truncated", truncated),
	)
}
