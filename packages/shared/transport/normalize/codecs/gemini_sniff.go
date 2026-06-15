package codecs

import (
	"bytes"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// LooksLike implements core.Sniffer: matches the Gemini generateContent
// wire by its `"candidates"` discriminator PLUS a corroborating Gemini
// response key within the probe window. "candidates" alone is too
// generic — any JSON API returning a list of options (an election
// service, a recruiting tool) carries it, and a single-key probe would
// steal that traffic from the generic-http projection. A real Gemini
// chunk always carries at least one of `usageMetadata` (every final
// chunk / non-stream body), `finishReason` (every terminal candidate),
// or `content` (every content-bearing candidate) right next to the
// candidates array, so requiring a second marker costs no recall.
// Both the plain JSON body and the SSE form (`data: {"candidates"`)
// carry the keys inside the probe window, and Google emits them with
// or without a space after the colon, so key-only Contains probes are
// the precise form. The cross-corpus sniffer matrix test pins both
// precision and recall.
//
// Request direction (or direction unset): `"contents"` plus one
// corroborating Gemini request key — `"generationConfig"`,
// `"systemInstruction"`, or `"safetySettings"` (camelCase keys no
// other wire ships). "contents" alone is the same generic-word trap
// as "candidates": any document API carries it. A minimal request
// with only `contents` is therefore not sniffed — it falls to the
// pattern probe / verbatim tiers (precision over recall).
func (n *GeminiGenerateNormalizer) LooksLike(raw []byte, meta core.Meta) bool {
	probe := sniffProbe(raw)
	if meta.Direction != core.DirectionResponse &&
		bytes.Contains(probe, []byte(`"contents"`)) &&
		(bytes.Contains(probe, []byte(`"generationConfig"`)) ||
			bytes.Contains(probe, []byte(`"systemInstruction"`)) ||
			bytes.Contains(probe, []byte(`"safetySettings"`))) {
		return true
	}
	if !bytes.Contains(probe, []byte(`"candidates"`)) {
		return false
	}
	return bytes.Contains(probe, []byte(`"usageMetadata"`)) ||
		bytes.Contains(probe, []byte(`"finishReason"`)) ||
		bytes.Contains(probe, []byte(`"content"`))
}
