package codec

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"

	"github.com/tidwall/gjson"
)

// Content-part helpers shared by the Gemini chat / image / video codecs:
// data-URL parsing, remote-URL mimeType guessing, and OpenAI content
// stringification. Split out of codec.go along the request-encoding seam
// so the codec file stays under the file-size ratchet.

// GuessMimeFromURL maps a remote URL to a Gemini mimeType by extension, after
// stripping any query string or fragment.
//
// `fallback` is what an unrecognised extension becomes, and it is the CALLER's
// to supply because only the caller knows what kind of part it is holding. The
// previous single default of image/jpeg was right for an image_url and wrong
// everywhere else it was used: a hosted document arrived at Gemini labelled as
// a JPEG, which asks the model to decode a PDF as a photograph.
//
// The table is per media kind rather than image-only for the same reason — an
// extension this function does not know is a guess, but an extension it does
// know should not be answered with the wrong family.
func GuessMimeFromURL(u, fallback string) string {
	lower := strings.ToLower(u)
	if i := strings.Index(lower, "?"); i >= 0 {
		lower = lower[:i]
	}
	if i := strings.Index(lower, "#"); i >= 0 {
		lower = lower[:i]
	}
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".heic"):
		return "image/heic"
	case strings.HasSuffix(lower, ".heif"):
		return "image/heif"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(lower, ".mov"):
		return "video/quicktime"
	case strings.HasSuffix(lower, ".webm"):
		return "video/webm"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	case strings.HasSuffix(lower, ".md"):
		return "text/markdown"
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	default:
		return fallback
	}
}

func StringifyContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var buf string
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				if buf != "" {
					buf += "\n"
				}
				buf += part.Get("text").String()
			}
			return true
		})
		return buf
	}
	return ""
}

// videoPart builds the Gemini part for a canonical video_url.
//
// Inline bytes and a hosted URI are the same split images and documents use;
// this lives beside GuessMimeFromURL because the fallback it passes is the
// whole reason that function takes one.
func videoPart(url string) (map[string]any, error) {
	if url == "" {
		return nil, errUnsupportedField("video_url.url")
	}
	if strings.HasPrefix(url, "data:") {
		media, b64, ok := specutil.ParseDataURL(url)
		if !ok {
			return nil, errUnsupportedField("video_url.url(data:invalid)")
		}
		return map[string]any{
			"inlineData": map[string]any{"mimeType": media, "data": b64},
		}, nil
	}
	return map[string]any{
		"fileData": map[string]any{
			"mimeType": GuessMimeFromURL(url, "video/mp4"),
			"fileUri":  url,
		},
	}, nil
}
