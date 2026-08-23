package codec

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

func imagePart(mediaType string) gjson.Result {
	data := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
	return gjson.Parse(`[{"type":"image_url","image_url":{"url":"data:` + mediaType +
		`;base64,` + data + `"}}]`)
}

// The Messages API takes four image types and nothing else. Which four is
// measured, not assumed: against api.anthropic.com, image/png answers 200 while
// image/heic and image/svg+xml answer
// "media_type: Input should be 'image/jpeg', 'image/png', 'image/gif' or
// 'image/webp'".
//
// Forwarding that refusal is what shipped, and it is a bad answer for a caller:
// it names the vendor's enum, not their attachment. Someone who sent a HEIC
// photo from an iPhone gets four strings and no indication of which field, which
// image, or that converting it is the fix.
func TestImageMediaType_UnsupportedFormatsAreRefusedByName(t *testing.T) {
	for _, mediaType := range []string{
		"image/heic", "image/svg+xml", "image/bmp", "image/tiff", "application/pdf",
	} {
		t.Run(mediaType, func(t *testing.T) {
			_, err := openAIPartsToAnthropicContent(imagePart(mediaType))
			if err == nil {
				t.Fatalf("%s was encoded; the wire answers 400 for it and the caller learns "+
					"nothing about their own image", mediaType)
			}
			var pe *provcore.ProviderError
			if !errors.As(err, &pe) || pe.Status != 400 {
				t.Fatalf("err = %v, want a 400 ProviderError", err)
			}
			if !strings.Contains(pe.Message, mediaType) {
				t.Errorf("message %q does not name the type that was sent", pe.Message)
			}
			if !strings.Contains(pe.Message, "image/png") {
				t.Errorf("message %q does not say what WOULD work, which is the only "+
					"actionable half", pe.Message)
			}
		})
	}
}

// The four the wire does take must still reach it.
func TestImageMediaType_SupportedFormatsReachTheWire(t *testing.T) {
	for _, mediaType := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		t.Run(mediaType, func(t *testing.T) {
			parts, err := openAIPartsToAnthropicContent(imagePart(mediaType))
			if err != nil {
				t.Fatalf("%s was refused: %v", mediaType, err)
			}
			if len(parts) != 1 {
				t.Fatalf("got %d parts, want 1", len(parts))
			}
			src, _ := parts[0]["source"].(map[string]any)
			if got := src["media_type"]; got != mediaType {
				t.Errorf("media_type = %v, want %s", got, mediaType)
			}
		})
	}
}

// A parameter or surrounding whitespace on an otherwise-supported type is not a
// format the wire refuses — it is a form WE were emitting. Measured: both
// "image/png; charset=utf-8" and " image/png " answer 400 with the same enum
// message as a genuinely unsupported format, which is what made this class look
// like a capability limit. The shared data-URL parser normalizes the type, so a
// caller writing either one reaches the wire.
func TestImageMediaType_ParametersAndSpaceAreNormalizedNotRefused(t *testing.T) {
	for _, declared := range []string{"image/png; charset=utf-8", " image/png ", "IMAGE/PNG"} {
		t.Run(declared, func(t *testing.T) {
			parts, err := openAIPartsToAnthropicContent(imagePart(declared))
			if err != nil {
				t.Fatalf("%q was refused (%v) — the caller's image is a PNG and the wire takes "+
					"PNGs; only our spelling of the type was in the way", declared, err)
			}
			src, _ := parts[0]["source"].(map[string]any)
			if got, _ := src["media_type"].(string); !strings.EqualFold(got, "image/png") {
				t.Errorf("media_type = %q, want image/png", got)
			}
		})
	}
}
