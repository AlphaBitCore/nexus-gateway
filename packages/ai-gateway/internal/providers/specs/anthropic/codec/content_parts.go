// Content-part translation between the canonical OpenAI chat content array and
// Anthropic's content blocks: text, image, document, tool_result.
package codec

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// Without this a non-Anthropic ingress rebuilds each part as {type, text} and
// the marker is gone before the wire-rewrite injector can see it — measured on
// production, the same marker cached on /v1/messages (cache_creation_tokens
// 12924, provider_cache_status miss) and did nothing on /v1/chat/completions
// (provider_cache_status na).
//
// Forwarded verbatim rather than validated: it is Anthropic's field on
// Anthropic's wire. Anthropic accepts cache_control on text, image, document
// and tool_result blocks alike, so this applies to whatever the part became.
func carryCacheControl(block map[string]any, part gjson.Result) map[string]any {
	if cc := part.Get("cache_control"); cc.Exists() {
		block["cache_control"] = cc.Value()
	}
	return block
}

func openAIPartsToAnthropicContent(content gjson.Result) ([]map[string]any, error) {
	var parts []map[string]any
	var err error
	content.ForEach(func(_, part gjson.Result) bool {
		if err != nil {
			return false
		}
		switch part.Get("type").String() {
		case "text":
			parts = append(parts, carryCacheControl(map[string]any{"type": "text", "text": part.Get("text").String()}, part))
		case "image_url":
			detail := part.Get("image_url.detail").String()
			if detail == "high" {
				err = errUnsupportedField("image_url.detail=high")
				return false
			}
			url := part.Get("image_url.url").String()
			if url == "" {
				err = errUnsupportedField("image_url.url")
				return false
			}
			if strings.HasPrefix(url, "data:") {
				media, b64, ok := specutil.ParseDataURL(url)
				if !ok {
					err = errUnsupportedField("image_url.url(data:invalid)")
					return false
				}
				if !anthropicImageMediaTypes[media] {
					err = errUnsupportedImageType(media)
					return false
				}
				parts = append(parts, carryCacheControl(map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": media,
						"data":       b64,
					},
				}, part))
			} else {
				parts = append(parts, carryCacheControl(map[string]any{
					"type": "image",
					"source": map[string]any{
						"type": "url",
						"url":  url,
					},
				}, part))
			}
		case "file":
			// Without this case the default branch forwards a chat-shaped
			// {"type":"file"} to the Messages API, which rejects it.
			file := part.Get("file")
			switch {
			case file.Get("file_id").Exists():
				parts = append(parts, carryCacheControl(map[string]any{
					"type":   "document",
					"source": map[string]any{"type": "file", "file_id": file.Get("file_id").String()},
				}, part))
			case strings.HasPrefix(file.Get("file_data").String(), "data:"):
				media, b64, ok := specutil.ParseDataURL(file.Get("file_data").String())
				if !ok {
					err = errUnsupportedField("file.file_data(data:invalid)")
					return false
				}
				block, berr := anthropicDocumentBlock(media, b64)
				if berr != nil {
					err = berr
					return false
				}
				parts = append(parts, carryCacheControl(block, part))
			case file.Get("file_url").Exists():
				parts = append(parts, carryCacheControl(map[string]any{
					"type":   "document",
					"source": map[string]any{"type": "url", "url": file.Get("file_url").String()},
				}, part))
			default:
				err = errUnsupportedField("file(no file_id, data: URL or file_url)")
				return false
			}
		case "tool_result":
			parts = append(parts, carryCacheControl(map[string]any{
				"type":        "tool_result",
				"tool_use_id": part.Get("tool_call_id").String(),
				"content":     StringifyOpenAIToolResultContent(part.Get("content")),
			}, part))
		case "video_url":
			// Forwarding earns the caller Anthropic's discriminated-union
			// error: "Input tag 'video_url' found using 'type' does not match
			// any of the expected tags: 'bash_code_execution_tool_result', …".
			// Eighteen production refusals were this.
			err = errUnsupportedVideo()
			return false
		default:
			// Unknown kinds ride through for forward compatibility with blocks
			// Anthropic adds; kinds known to be refused are named above.
			var m map[string]any
			if uerr := json.Unmarshal([]byte(part.Raw), &m); uerr == nil {
				parts = append(parts, m)
			}
		}
		return true
	})
	return parts, err
}

func StringifyOpenAIToolResultContent(c gjson.Result) string {
	if c.Type == gjson.String {
		return c.String()
	}
	return c.Raw
}

// anthropicDocumentBlock: a base64 source is PDF-ONLY. Measured against
// api.anthropic.com with the same markdown bytes, as a base64 source it answers
// "document.source.base64.media_type: Input should be 'application/pdf'" and as
// a text source it answers 200 having read the document. Anthropic's model
// listing publishes capabilities{image_input, pdf_input} and says nothing about
// text documents while the wire plainly reads one — a capability list is a floor.
func anthropicDocumentBlock(media, b64 string) (map[string]any, error) {
	if media == "application/pdf" {
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":       "base64",
				"media_type": media,
				"data":       b64,
			},
		}, nil
	}
	if !specutil.IsTextualMediaType(media) {
		return nil, errUnsupportedDocumentType(media)
	}
	raw, decErr := base64.StdEncoding.DecodeString(b64)
	if decErr != nil {
		// ParseDataURL re-emits standard padded base64, so reaching here means
		// the payload was not what it claimed.
		return nil, errUnsupportedField("file.file_data(base64 undecodable)")
	}
	if !utf8.Valid(raw) {
		return nil, errUnsupportedField("file.file_data(declared " + media +
			" but the bytes are not valid UTF-8)")
	}
	// The text source takes text/plain and no other media type: forwarding the
	// caller's more specific type (text/markdown, application/json) is refused
	// by the wire for a document it would otherwise read.
	return map[string]any{
		"type": "document",
		"source": map[string]any{
			"type":       "text",
			"media_type": "text/plain",
			"data":       string(raw),
		},
	}, nil
}

func errUnsupportedVideo() error {
	return &provcore.ProviderError{
		Status: http.StatusBadRequest,
		Code:   provcore.CodeInvalidRequest,
		Type:   "nexus_field_unsupported",
		Message: "nexus: the Anthropic Messages API has no video part — it carries text, " +
			"images and documents only; extract a frame as an image, or route to a model on " +
			"a wire that reads video",
	}
}

func errUnsupportedDocumentType(mediaType string) error {
	return &provcore.ProviderError{
		Status: http.StatusBadRequest,
		Code:   provcore.CodeInvalidRequest,
		Type:   "nexus_field_unsupported",
		Message: "nexus: the Anthropic Messages API reads a document as a PDF or as text, " +
			"and this document is " + mediaType +
			" — send it as application/pdf or as a text type (text/plain, text/markdown, " +
			"application/json), or route to a model on a wire that reads it",
	}
}

// The COMPLEMENT OF THE MEASURED REFUSALS, not a published list: measured
// against api.anthropic.com in Anthropic's own image/source shape, jpeg, png,
// gif and webp each returned 200 with the number rendered in the pixels, and
// bmp, tiff, heic and svg+xml each returned 400 — "media_type: Input should be
// 'image/jpeg', 'image/png', 'image/gif' or 'image/webp'". A format merely
// never seen to work is not a format shown not to work.
//
// A well-formed type carrying a parameter or surrounding whitespace draws the
// same 400; the shared data-URL parser normalizes those before this map is
// consulted.
var anthropicImageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

func errUnsupportedImageType(mediaType string) error {
	return &provcore.ProviderError{
		Status: http.StatusBadRequest,
		Code:   provcore.CodeInvalidRequest,
		Type:   "nexus_field_unsupported",
		Message: "nexus: the Anthropic Messages API accepts image/jpeg, image/png, image/gif " +
			"and image/webp, and this image is " + mediaType +
			" — convert it before sending, or route to a model on a wire that reads it",
	}
}
