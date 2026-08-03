package codecs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// OpenAIImagesNormalizer handles OpenAI's /v1/images/generations surface for
// both directions. It exists so the Traffic drawer shows the image PROMPT as
// scannable user text and summarizes the response artifact by size/mime
// instead of the Tier-3 fallback dumping the multi-MB base64 payload inline.
//
// Request shape (application/json):
//
//	{"model":"dall-e-3","prompt":"a red fox","n":1,"size":"1024x1024",...}
//
// The only user-text field is `prompt` (string or []string); n/size/quality/
// style/response_format/user are non-content knobs. Kind = ai-image.
//
// Response shape:
//
//	{"created":..,"data":[{"b64_json":"..","revised_prompt":".."}|{"url":".."}]}
//
// revised_prompt (the provider's rewritten prompt) is projected as assistant
// text; each artifact becomes an image_ref summary (b64 decoded byte size +
// sniffed mime, NEVER the bytes) or, for url mode, an inert text block carrying
// the reference (BinaryRef has no url field, and a plain-text block renders
// non-clickable — no auto-fetch, no anchor). Kind = ai-image.
type OpenAIImagesNormalizer struct{}

// NewOpenAIImagesNormalizer returns a stateless normalizer instance.
func NewOpenAIImagesNormalizer() *OpenAIImagesNormalizer { return &OpenAIImagesNormalizer{} }

// ID is the metric / log label.
func (n *OpenAIImagesNormalizer) ID() string { return "openai-images" }

// Normalize routes by Meta.Direction. Both directions are JSON — there is no
// multipart request codec (edits/variations are deferred and multipart).
func (n *OpenAIImagesNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroImagePayload(meta), fmt.Errorf("openai-images: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroImagePayload(meta), fmt.Errorf("openai-images: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, openAIImagesFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "openai-images"
		}
	}
	return p, err
}

func openAIImagesFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"prompt"},
			Optional: []string{"model", "n", "size", "quality", "style", "response_format", "user", "background"},
		}
	}
	return core.FieldSpec{
		Required: []string{"data"},
		Optional: []string{"created", "background", "usage"},
	}
}

// openAIImagesRequest mirrors the request body. `prompt` is polymorphic
// (string or []string), so it is captured raw and decoded like embeddings.
type openAIImagesRequest struct {
	Model  string          `json:"model"`
	Prompt json.RawMessage `json:"prompt"`
}

func (n *OpenAIImagesNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req openAIImagesRequest
	if err := decodeLenient(raw, &req); err != nil {
		return zeroImagePayload(meta), fmt.Errorf("openai-images: request unmarshal: %w", err)
	}
	if len(req.Prompt) == 0 || string(req.Prompt) == "null" {
		return zeroImagePayload(meta), fmt.Errorf("openai-images: missing prompt: %w", core.ErrUnsupported)
	}
	texts, err := decodeStringOrStringArray(req.Prompt)
	if err != nil {
		return zeroImagePayload(meta), fmt.Errorf("openai-images: prompt decode: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIImage,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-images",
		Model:            firstNonEmpty(req.Model, meta.Model),
	}
	if len(texts) > 0 {
		out.Messages = []core.Message{{Role: core.RoleUser, Content: textBlocks(texts)}}
	}
	return out, nil
}

// openAIImagesResponse mirrors the response body. Only metadata is parsed;
// the b64_json bytes are summarized, never inlined.
type openAIImagesResponse struct {
	Data []openAIImageDatum `json:"data"`
}

type openAIImageDatum struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

func (n *OpenAIImagesNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp openAIImagesResponse
	if err := decodeLenient(raw, &resp); err != nil {
		return zeroImagePayload(meta), fmt.Errorf("openai-images: response unmarshal: %w", err)
	}
	if resp.Data == nil {
		return zeroImagePayload(meta), fmt.Errorf("openai-images: response missing data: %w", core.ErrUnsupported)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIImage,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-images",
		Model:            meta.Model,
	}
	var blocks []core.ContentBlock
	for _, d := range resp.Data {
		if d.RevisedPrompt != "" {
			blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: d.RevisedPrompt})
		}
		switch {
		case d.B64JSON != "":
			size, mime := b64ArtifactSummary(d.B64JSON)
			blocks = append(blocks, core.ContentBlock{
				Type:     core.ContentImageRef,
				ImageRef: &core.BinaryRef{Size: size, ContentType: mime},
			})
		case d.URL != "":
			// BinaryRef has no url field; carry the reference as an inert
			// text block (renders non-clickable — no auto-fetch, no anchor,
			// no server-side deref).
			blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: "image url: " + d.URL})
		}
	}
	if len(blocks) > 0 {
		out.Messages = []core.Message{{Role: core.RoleAssistant, Content: blocks}}
	}
	return out, nil
}

// b64ArtifactSummary returns the decoded byte size and a sniffed mime for a
// base64 image artifact WITHOUT allocating the whole decoded blob: the size is
// computed arithmetically, and only a short prefix is decoded to sniff the mime.
func b64ArtifactSummary(b64 string) (int64, string) {
	size := int64(base64.StdEncoding.DecodedLen(len(b64)))
	// DecodedLen is an upper bound (it ignores padding); subtract the '='
	// padding bytes for the exact size.
	for i := len(b64) - 1; i >= 0 && b64[i] == '='; i-- {
		size--
	}
	if size < 0 {
		size = 0
	}
	mime := "application/octet-stream"
	// Decode a bounded prefix (16 b64 chars → 12 bytes) to sniff magic bytes.
	prefixLen := len(b64)
	if prefixLen > 16 {
		prefixLen = 16
	}
	if head, err := base64.StdEncoding.DecodeString(b64[:prefixLen]); err == nil {
		mime = sniffImageMagic(head)
	} else {
		// The prefix is not valid base64, so the arithmetic size is a fiction —
		// report 0 rather than a fabricated byte count for a malformed artifact.
		size = 0
	}
	return size, mime
}

// sniffImageMagic returns the image mime for the common web image magic bytes,
// falling back to net/http content sniffing. Kept local so the shared codec
// package has no gateway-internal dependency.
func sniffImageMagic(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	}
	if ct := http.DetectContentType(b); ct != "application/octet-stream" {
		return ct
	}
	return "application/octet-stream"
}

func zeroImagePayload(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAIImage,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-images",
		Model:            meta.Model,
	}
}

// decodeStringOrStringArray decodes a JSON value that is either a string or an
// array of strings into a []string, dropping empty elements. Shared by the
// image (prompt) and tts (input) codecs.
func decodeStringOrStringArray(raw json.RawMessage) ([]string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	var ss []string
	if err := json.Unmarshal(raw, &ss); err == nil {
		out := make([]string, 0, len(ss))
		for _, e := range ss {
			if e != "" {
				out = append(out, e)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unrecognised text shape: %w", core.ErrUnsupported)
}

// textBlocks maps strings to text ContentBlocks.
func textBlocks(texts []string) []core.ContentBlock {
	blocks := make([]core.ContentBlock, 0, len(texts))
	for _, t := range texts {
		blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: t})
	}
	return blocks
}
