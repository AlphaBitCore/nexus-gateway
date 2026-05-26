package wireformat

import (
	"errors"
	"fmt"

	"github.com/tidwall/gjson"
)

// ErrGeminiChunkShape is returned when a streamed GenerateContentResponse JSON
// object does not match the minimal shape described in Gemini API docs.
var ErrGeminiChunkShape = errors.New("wireformat: invalid Gemini GenerateContentResponse chunk shape")

// ErrGeminiRequestShape is returned when a generateContent request body does
// not match the minimal documented request envelope.
var ErrGeminiRequestShape = errors.New("wireformat: invalid Gemini generateContent request shape")

// ValidateGeminiGenerateContentResponseChunk validates one SSE JSON payload for
// streamGenerateContent (?alt=sse). Chunks may omit candidates when only
// usageMetadata is present (trailer patterns vary by model).
func ValidateGeminiGenerateContentResponseChunk(data []byte) error {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return fmt.Errorf("%w: empty json", ErrGeminiChunkShape)
	}
	if !root.Get("candidates").Exists() {
		return nil
	}
	arr := root.Get("candidates").Array()
	if len(arr) == 0 {
		return nil
	}
	for i, c := range arr {
		if !c.Get("content").Exists() {
			return fmt.Errorf("%w: candidates[%d] missing content", ErrGeminiChunkShape, i)
		}
		parts := c.Get("content.parts")
		if !parts.Exists() || len(parts.Array()) == 0 {
			return fmt.Errorf("%w: candidates[%d] missing content.parts", ErrGeminiChunkShape, i)
		}
	}
	return nil
}

// ValidateGeminiGenerateContentRequest checks a non-streaming generateContent
// request body for fields commonly present in public examples (contents array,
// optional generationConfig, optional safetySettings).
func ValidateGeminiGenerateContentRequest(data []byte) error {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return fmt.Errorf("%w: empty json", ErrGeminiRequestShape)
	}
	contents := root.Get("contents")
	if !contents.Exists() || !contents.IsArray() || len(contents.Array()) == 0 {
		return fmt.Errorf("%w: contents must be a non-empty array", ErrGeminiRequestShape)
	}
	if gc := root.Get("generationConfig"); gc.Exists() && !gc.IsObject() {
		return fmt.Errorf("%w: generationConfig must be an object when present", ErrGeminiRequestShape)
	}
	if ss := root.Get("safetySettings"); ss.Exists() && !ss.IsArray() {
		return fmt.Errorf("%w: safetySettings must be an array when present", ErrGeminiRequestShape)
	}
	return nil
}
