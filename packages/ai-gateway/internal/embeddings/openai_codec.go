package embeddings

// no streaming — embeddings endpoint is request/response only.

import (
	"encoding/base64"
	"fmt"
	"github.com/goccy/go-json"
	"math"

	"github.com/tidwall/gjson"
)

// openAIEmbeddingWireRequest is the JSON-marshallable struct for the
// OpenAI /v1/embeddings request body. Pointer fields are omitted from
// the output when nil, which is how optional parameters (dimensions,
// encoding_format) are expressed without dead error-handling branches.
type openAIEmbeddingWireRequest struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	EncodingFormat string `json:"encoding_format,omitempty"`
}

// EncodeOpenAIRequest serialises req into the OpenAI /v1/embeddings
// JSON wire format.
//
// Required fields: model, input.  Input is always a string here (the L2
// cache hot path embeds exactly one text per call); cross-format batch
// embedding goes through providers/specs/openai/codec/embeddings.go.
// Optional fields: dimensions (omitted when 0) and encoding_format
// (omitted when "").
func EncodeOpenAIRequest(req Request) ([]byte, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("embeddings: EncodeOpenAIRequest: model is required")
	}
	if req.Input == "" {
		return nil, fmt.Errorf("embeddings: EncodeOpenAIRequest: input is required")
	}

	wire := openAIEmbeddingWireRequest{
		Model:          req.Model,
		Input:          req.Input,
		EncodingFormat: req.EncodingFormat,
	}
	if req.Dimensions > 0 {
		d := req.Dimensions
		wire.Dimensions = &d
	}

	return json.Marshal(wire)
}

// DecodeOpenAIResponse parses the OpenAI /v1/embeddings response body
// and returns a Response.
//
// Expected shape:
//
//	{
//	  "object": "list",
//	  "data": [{"object":"embedding","embedding":[...],"index":0}],
//	  "model": "text-embedding-3-small",
//	  "usage": {"prompt_tokens": N, "total_tokens": N}
//	}
//
// The embedding field inside data[0] may be a JSON array of floats
// (encoding_format="float") or a base64-encoded binary string
// (encoding_format="base64"). The format is detected from the raw JSON
// type — array → float path, string → base64 path.
//
// Returns an error if the data array is empty or if a base64 blob
// cannot be decoded.
func DecodeOpenAIResponse(body []byte) (Response, error) {
	if !gjson.ValidBytes(body) {
		return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: invalid JSON response")
	}

	dataArr := gjson.GetBytes(body, "data")
	if !dataArr.Exists() || !dataArr.IsArray() || len(dataArr.Array()) == 0 {
		return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: data array is missing or empty")
	}

	first := dataArr.Array()[0]
	embVal := first.Get("embedding")
	if !embVal.Exists() {
		return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: data[0].embedding missing")
	}

	var embedding []float32

	switch embVal.Type {
	case gjson.JSON:
		// Float array path.
		if !embVal.IsArray() {
			return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: data[0].embedding is not an array")
		}
		arr := embVal.Array()
		embedding = make([]float32, len(arr))
		for i, v := range arr {
			f := v.Float()
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: data[0].embedding[%d] is not a finite number", i)
			}
			embedding[i] = float32(f)
		}
	case gjson.String:
		// Base64 path (encoding_format="base64").
		raw, err := base64.StdEncoding.DecodeString(embVal.String())
		if err != nil {
			return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: base64 decode: %w", err)
		}
		// OpenAI base64 embeddings are little-endian IEEE 754 float32.
		if len(raw)%4 != 0 {
			return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: base64 payload length %d is not a multiple of 4", len(raw))
		}
		embedding = decodeFloat32LE(raw)
	default:
		return Response{}, fmt.Errorf("embeddings: DecodeOpenAIResponse: data[0].embedding has unexpected JSON type %s", embVal.Type)
	}

	model := gjson.GetBytes(body, "model").String()
	promptTokens := int(gjson.GetBytes(body, "usage.prompt_tokens").Int())

	return Response{
		Embedding:    embedding,
		Model:        model,
		PromptTokens: promptTokens,
	}, nil
}

// decodeFloat32LE converts a byte slice of little-endian IEEE 754
// float32 values (4 bytes each) into a []float32.
func decodeFloat32LE(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		bits := uint32(b[i*4]) |
			uint32(b[i*4+1])<<8 |
			uint32(b[i*4+2])<<16 |
			uint32(b[i*4+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// openAIEmbeddingError parses the OpenAI error envelope from a non-2xx
// response body and returns a descriptive string. Used by Client.Embed.
func openAIEmbeddingError(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// Try structured envelope: {"error":{"message":"..."}}
	msg := gjson.GetBytes(body, "error.message").String()
	if msg != "" {
		return msg
	}
	// Fall back to the raw JSON (truncated) for observability.
	const maxRaw = 256
	if len(body) > maxRaw {
		body = body[:maxRaw]
	}
	// Confirm it's printable JSON before returning raw.
	var v any
	if json.Unmarshal(body, &v) == nil {
		return string(body)
	}
	return fmt.Sprintf("unparseable error body (%d bytes)", len(body))
}
