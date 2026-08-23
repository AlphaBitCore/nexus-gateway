package moonshot

// Moonshot is the OpenAI-compatible shape except for content parts. Measured
// against api.moonshot.cn:
//
//   - images are read from INLINE BYTES ONLY. A data: URL answers 200 and
//     describes the image ("Red square."); a reachable https URL answers 400
//     "unsupported image url: <url>", and so does an unreachable one — the
//     refusal is categorical.
//   - a `file` part: "the message at position 0 with role 'user' contains an
//     invalid part type: file". The Files API (GET /v1/files answers 200) is
//     upload-then-reference, so there is no inline document form to map onto.

import "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"

func contentPolicy() specutil.ContentPolicy {
	return specutil.ContentPolicy{
		Allow: map[string]bool{"image_url": true},
		Deny: map[string]string{
			"file": "Moonshot chat has no inline document part. Its Files API is " +
				"upload-then-reference, which this gateway does not perform, so send the " +
				"document's text in the message instead",
			"input_audio": "Moonshot chat does not accept audio content",
		},
		InlineOnlyImageURL: "Moonshot reads images from inline bytes only, so an image_url must " +
			"be a data: URL carrying the image (data:image/png;base64,…); this wire does not " +
			"fetch images by URL",
		// The complement of the measured REFUSALS, not the measured reads: a
		// format that merely failed to produce an anchor is not a reason to
		// refuse it. Copying OpenAI's png/jpeg/gif/webp here by analogy would
		// have refused BMP, which this wire reads.
		//
		// Measured: png, jpeg, gif, webp and bmp read the image; heic is
		// accepted; svg answers "unsupported image format: text/plain;
		// charset=utf-8" (it content-sniffs) and tiff is refused outright.
		ImageFormats: map[string]bool{
			"image/png": true, "image/jpeg": true, "image/gif": true,
			"image/webp": true, "image/bmp": true, "image/heic": true,
		},
	}
}
