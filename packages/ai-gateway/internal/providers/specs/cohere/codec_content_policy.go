package cohere

// codec_content_policy.go — the content-part limits of Cohere's chat wire.
//
// The document side is handled in codec_content.go, which lifts a text file
// part onto the top-level `documents` array and refuses anything binary in its
// own words. This adds the two limits that belong to the shared gate:
//
//   - the image formats, which Cohere's own refusal names as a closed set:
//     "unsupported image file format, only PNG, JPEG, WebP, and GIF are
//     supported". A closed set the wire publishes is a set we can hold — and
//     the complement was measured rather than assumed: bmp, tiff, heic and
//     svg+xml each answer 400 against api.cohere.com directly, while all four
//     accepted formats returned the number rendered in the pixels. A whitelist
//     decides what WE turn away, so it is built from the refusals; a format
//     that was merely never seen to work has not been shown not to work.
//   - an attachment declared application/octet-stream, which is not a Cohere
//     limitation at all — it is the caller not saying what the file is.

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

func contentPolicyFor(provcore.CallTarget) specutil.ContentPolicy {
	return specutil.ContentPolicy{
		Allow: map[string]bool{"image_url": true, "file": true},
		Deny: map[string]string{
			"input_audio": "Cohere chat does not accept audio content",
			"video_url": "Cohere chat does not accept video content — send a frame as an " +
				"image, or route to a model on a wire that reads video",
		},
		ImageFormats: map[string]bool{
			"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
		},
	}
}
