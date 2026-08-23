package gemini

// codec_content.go — the content-part limits of Gemini's wire, declared as data.
//
// Gemini forwards the caller's declared media type verbatim into inlineData, so
// a type it cannot decode reaches the model as "Unsupported MIME type: X" —
// true, and useless to a caller who did not choose that type.
//
// HOW ImageFormats BELOW WAS BUILT, because the construction is the part that
// goes wrong. It is the COMPLEMENT OF THE MEASURED REFUSALS, not the set of
// formats measured to read. Every format was posted to
// generativelanguage.googleapis.com directly, in Google's own inline_data
// shape, so that our own gate could not answer first:
//
//	png, jpeg, gif, webp   200, and the number rendered in the pixels came back
//	heic                   200 — accepted, wrong answer on a fixture with no anchor
//	bmp, tiff, svg+xml     400 "Unsupported MIME type: image/<type>"
//
// Neither available source could have produced that list. Google's
// documentation names png, jpeg, webp, heic and heif — and NOT gif, which this
// wire demonstrably reads; a published list is a floor. And building the
// allow-list from the formats that produced a correct answer would have dropped
// heic, which the wire accepted: a fixture with no anchor cannot show a format
// is unreadable, and refusing what works is worse than forwarding what does
// not. heif is carried on the documentation's word for the same reason — it was
// never measured as refused, so refusing it would be a guess against the caller.
//
// Unlike OpenAI, Anthropic and Cohere, whose refusals enumerate the accepted set
// in the error itself, Gemini's names only the offending type. There was no list
// to read off a single 400 here; it had to be swept format by format.

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

func contentPolicyFor(target provcore.CallTarget) specutil.ContentPolicy {
	// Video is carried, and that is measured rather than assumed. A 7.9 KB mp4
	// sent as inlineData with mimeType video/mp4, straight at
	// generativelanguage.googleapis.com: gemini-2.5-flash answered 200 and began
	// describing the footage, and gemini-3.5-flash answered 200 with the input
	// accounted under promptTokensDetails as modality VIDEO — the provider
	// billing it as video is the strongest available statement that the part was
	// read.
	//
	// The refusal that stood here cited no measurement, alone among the entries
	// in this file, and refusing what works is the one direction the rule above
	// forbids.
	p := specutil.ContentPolicy{
		Allow: map[string]bool{
			"image_url": true, "file": true, "input_audio": true, "video_url": true,
		},
	}
	// PER MODEL, not per provider. The generations decode different sets, and a
	// list measured on one of them takes away what the other reads.
	//
	// Measured directly, every model in the catalog, one probe each:
	//
	//   2.5-flash / 2.5-flash-lite / 2.5-pro   bmp, tiff, svg+xml -> 400
	//   3.1-flash-lite / 3.5-flash             bmp, tiff -> 200 with the number
	//                                          rendered in the pixels; svg -> 200
	//
	// So generation 3 refuses nothing that was measured, and gets NO list: an
	// allow-list there would refuse formats this wire demonstrably reads, which
	// is the one direction the construction rule forbids. Generation 2.5 keeps
	// the complement of its own measured refusals.
	//
	// Shipping a single provider-wide list built from 2.5-flash alone is exactly
	// how bmp and tiff started being refused on the 3.x models that read them.
	if !specutil.GenerationAtLeast(target.ProviderModelID, "gemini-", 3) {
		p.ImageFormats = map[string]bool{
			"image/png":  true,
			"image/jpeg": true,
			"image/gif":  true, // measured; absent from Google's published list
			"image/webp": true,
			"image/heic": true,
			"image/heif": true, // documented, never measured as refused
		}
	}
	return p
}
