package codec

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Content-part helpers shared by the Gemini chat / image / video codecs:
// data-URL parsing, remote-URL mimeType guessing, and OpenAI content
// stringification. Split out of codec.go along the request-encoding seam
// so the codec file stays under the file-size ratchet.

// ParseDataURL extracts the media type and base64 payload from a data: URL.
// Returns ok=false on shapes the codec cannot turn into a Gemini inlineData
// part (missing comma, missing ;base64, malformed payload). Exported for tests.
func ParseDataURL(dataURL string) (mediaType, b64 string, ok bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(dataURL, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 || comma == len(rest)-1 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return mediaType, payload, payload != ""
}

// GuessMimeFromURL maps a remote image URL to a best-effort Gemini mimeType
// by extension (after stripping any query string). Falls back to image/jpeg
// when the extension is missing or unrecognised so the upstream call still
// succeeds; operators can override via the Extras pipeline if a specific
// mimeType is required. Exported for tests.
func GuessMimeFromURL(u string) string {
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
	default:
		return "image/jpeg"
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
