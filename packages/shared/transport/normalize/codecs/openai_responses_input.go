// The Responses request `input` is the widest content union in this package:
// a bare string, a list of messages, or a list of typed parts, and the parts
// spell the same modality several ways (image_url as a bare string here where
// chat nests it in an object, file_data beside file_id, audio nested one
// level deeper than either). Reading it is a separate job from assembling the
// payload around it.

package codecs

import (
	"bytes"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// decodeOpenAIResponsesInput accepts the polymorphic `input` field —
// either a JSON string ("hello") or a JSON array of input items — and
// returns a uniform []openaiResponsesInputItem. ok=false means the raw
// bytes were empty / null / unrecognised shape; callers treat that as
// "no input items" (an instructions-only request, for example, is
// legal). String shorthand becomes a single user-role item with one
// input_text content block, matching how the gateway codec
// (packages/ai-gateway/internal/providers/specs/openai/responses/codec_responses.go:343)
// expands it into the canonical message list.
func decodeOpenAIResponsesInput(raw json.RawMessage) ([]openaiResponsesInputItem, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, false
		}
		return []openaiResponsesInputItem{{
			Role:    "user",
			Content: []openaiResponsesInputContent{{Type: "input_text", Text: s}},
		}}, true
	case '[':
		var items []openaiResponsesInputItem
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, false
		}
		return items, true
	}
	return nil, false
}

// base is the gjson path of the content array; empty means no positional
// context and media degrades to absent rather than pointing at the wrong place.
func openaiResponsesInputContentToBlocks(parts []openaiResponsesInputContent, base string) []core.ContentBlock {
	if len(parts) == 0 {
		return nil
	}
	out := make([]core.ContentBlock, 0, len(parts))
	for i, p := range parts {
		path := locator.JoinPath(base, i)
		switch p.Type {
		case "input_text", "":
			if p.Text != "" {
				out = append(out, core.ContentBlock{Type: core.ContentText, Text: p.Text})
			}
		case "input_image":
			// This used to emit an entirely empty ref — no mime, no size,
			// no source — which told a reader nothing except that something
			// had been there. All three documented shapes are real refs.
			switch {
			case p.ImageURL != "":
				out = append(out, mediaBlock(inlineOrExternal(
					p.ImageURL, locator.JoinSuffix(path, "image_url"), core.ModalityImage)))
			case p.FileID != "":
				out = append(out, mediaBlock(providerRefMedia("", p.FileID, core.ModalityImage)))
			default:
				out = append(out, mediaBlock(&core.MediaRef{Modality: core.ModalityImage, Source: core.MediaAbsent}))
			}
		case "input_audio":
			if p.InputAudio != nil && p.InputAudio.Data != "" {
				ref := capturedMedia(
					audioMimeFromFormat(p.InputAudio.Format),
					p.InputAudio.Data,
					locator.JSON(locator.JoinSuffix(path, "input_audio.data")))
				ref.Modality = core.ModalityAudio
				out = append(out, mediaBlock(ref))
			} else {
				out = append(out, mediaBlock(&core.MediaRef{Modality: core.ModalityAudio, Source: core.MediaAbsent}))
			}
		case "computer_screenshot":
			// Listed in the endpoint's own supported set. Dropping it took
			// the whole input item with it, since an item with no blocks is
			// skipped by the caller.
			switch {
			case p.ImageURL != "":
				out = append(out, mediaBlock(inlineOrExternal(
					p.ImageURL, locator.JoinSuffix(path, "image_url"), core.ModalityImage)))
			case p.FileID != "":
				out = append(out, mediaBlock(providerRefMedia("", p.FileID, core.ModalityImage)))
			default:
				out = append(out, mediaBlock(&core.MediaRef{Modality: core.ModalityImage, Source: core.MediaAbsent}))
			}
		case "input_file":
			// Precedence: inline bytes, then provider id, then remote URL.
			switch {
			case p.FileData != "":
				if mime, payload, isData := locator.ParseDataURI(p.FileData); isData {
					out = append(out, mediaBlock(capturedMedia(mime, payload, locator.DataURI(locator.JoinSuffix(path, "file_data")))))
				} else {
					out = append(out, mediaBlock(capturedMedia("", p.FileData, locator.JSON(locator.JoinSuffix(path, "file_data")))))
				}
			case p.FileID != "":
				out = append(out, mediaBlock(providerRefMedia("", p.FileID, core.ModalityFile)))
			case p.FileURL != "":
				out = append(out, mediaBlock(externalMedia("", p.FileURL, core.ModalityFile)))
			default:
				out = append(out, mediaBlock(&core.MediaRef{Modality: core.ModalityFile, Source: core.MediaAbsent}))
			}
		default:
			// Unknown part type — keep its text payload if any.
			if p.Text != "" {
				out = append(out, core.ContentBlock{Type: core.ContentText, Text: p.Text})
			}
		}
	}
	return out
}
