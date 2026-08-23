package openai

// codec_content.go — what OpenAI's chat wire will not carry, declared as data.
//
// OpenAI is the canonical shape, so its content parts pass through untouched
// and every refusal reaches the caller in OpenAI's vocabulary. Measured across
// the catalog, that is three distinct classes and ninety refusals:
//
//   - a `file` part carrying anything but a PDF: "Invalid file data:
//     'messages[0].content[0].file.file_data'. Expected a base64-encoded data
//     URL with an application/pdf MIME type … but got unsupported MIME type
//     'text/markdown'." The wire reads PDFs and nothing else as a document.
//   - an image that is not one of four formats: "You uploaded an unsupported
//     image. Please make sure your image has of one the following formats:
//     ['png', 'jpeg', 'gif', 'webp']."
//   - a video: "Invalid value: 'video_url'. Supported values are: 'text',
//     'image_url', 'input_audio', 'refusal', 'audio', and 'file'."
//
// The format list and the part list are OpenAI's own words from those errors,
// not a doc page — which is why they can be trusted to be current.
//
// The document case is the interesting one and it is NOT a refusal. A markdown
// or plain-text attachment is characters, and this wire reads characters
// perfectly well; it just will not take them wearing a document's clothes. So
// they ride as text and the conversion announces itself through x-nexus-coerced
// rather than happening silently.

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

func contentPolicy() specutil.ContentPolicy {
	return specutil.ContentPolicy{
		Allow: map[string]bool{
			"image_url":   true,
			"file":        true,
			"input_audio": true,
			"audio":       true,
			"refusal":     true,
		},
		Deny: map[string]string{
			"video_url": "the OpenAI chat wire has no video part — it carries text, images, " +
				"audio and documents only; extract a frame as an image, or route to a model " +
				"on a wire that reads video",
		},
		// Four formats, quoted from the wire's own refusal — and the complement
		// confirmed by posting each of bmp, tiff, heic and svg+xml straight to
		// api.openai.com, where all four answer 400. The enumerated set and the
		// measured refusals agree, which is what makes this list safe to hold:
		// a whitelist decides what WE turn away, so it is built from what the
		// wire was seen to refuse, never from what it was seen to read.
		ImageFormats: map[string]bool{
			"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
		},
		// This wire's audio models read a lone attachment only when a text part
		// precedes it — measured 6/6 against 0/6 on gpt-audio-mini and
		// gpt-audio-1.5, with HTTP 200 and a polite refusal on the losing order.
		TextPartLeadsAudio: "the OpenAI audio models read an attachment only when a text part " +
			"precedes it",
		// The document part is PDF-only here. Text attachments are not refused —
		// they are carried as text, which is the same characters by another
		// route.
		InlineTextDocuments: "the OpenAI chat wire reads a document only as a PDF",
	}
}

// contentPolicyFor is the resolver the spec installs.
//
// Uniform across the family for the content parts measured here: every OpenAI
// chat model that took an image took the same four formats, and every one that
// took a document took only PDF. Models differ in WHETHER they read images at
// all — o1 and the nano tiers do not — but that is a catalog question the
// routing guard answers before the codec is reached, not a content-shape one.
func contentPolicyFor(provcore.CallTarget) specutil.ContentPolicy { return contentPolicy() }
