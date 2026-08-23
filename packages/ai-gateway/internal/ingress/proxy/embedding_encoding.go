// Package proxy — embedding_encoding.go makes `encoding_format: "base64"` a
// gateway-level guarantee instead of a per-provider capability.
//
// Why this exists (AP-3): the official OpenAI SDKs send
// `encoding_format: "base64"` IMPLICITLY when the caller does not specify it —
// openai-node always, openai-python whenever it can decode the payload. Having
// asked for base64, the SDK then trusts the answer and decodes it. openai-node
// runs the moral equivalent of
//
//	Array.from(new Float32Array(Buffer.from(embedding, "base64").buffer))
//
// so a FLOAT ARRAY arriving where base64 was requested is not merely ignored —
// `Buffer.from([...])` truncates each number to one byte, and the reinterpret
// yields a vector of one QUARTER the length filled with garbage. Observed on
// staging 2026-07-27: a stock `embeddings.create({dimensions: 768})` against
// gemini-embedding-001 returned 192 numbers, no error. Silent corruption of a
// vector is worse than any 4xx: a caller's cosine similarity just quietly stops
// meaning anything.
//
// Only the OpenAI/Azure/DeepSeek codecs pass encoding_format to the wire;
// Voyage/Gemini/Bedrock always emit float, and Cohere's wire has no base64
// embedding type at all. So honouring the field cannot be left to the adapters.
// It is honoured HERE, once, on the client-facing response: canonical embeddings
// are always float arrays, and this re-encodes them to little-endian IEEE-754
// float32 base64 — byte-for-byte what api.openai.com returns — when the request
// asked for base64 and the body still carries arrays.
//
// This is the same adapter-fills-the-protocol-requirement pattern as the
// Anthropic `max_tokens` default: the caller's contract is satisfied by the
// gateway rather than pushed onto every provider codec.
package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"math"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// requestedEmbeddingEncodingFormat reads the encoding_format stamped by
// preStampEmbeddingRequestMeta. Returns "" when absent, which callers treat as
// "no re-encode" — the pre-stamp defaults it to "float" for every embeddings
// request, so "" only happens on a non-embeddings path.
func requestedEmbeddingEncodingFormat(existing any) string {
	md, ok := existing.(map[string]any)
	if !ok {
		return ""
	}
	emb, ok := md["embedding"].(map[string]any)
	if !ok {
		return ""
	}
	ef, _ := emb["encoding_format"].(string)
	return ef
}

// honorEmbeddingEncodingFormat re-encodes float-array embedding vectors to
// base64 when the request asked for base64 and the body still carries arrays.
//
// No-ops (returning respBody untouched) when:
//   - the request did not ask for base64 — float is already the wire default;
//   - the body carries base64 strings already (OpenAI-family upstream honoured
//     the field itself, so re-encoding would double-encode);
//   - the body has no data[] rows, or a row's embedding is neither array nor
//     string (nothing safe to convert — leave the payload alone rather than
//     risk mangling an unrecognised shape).
//
// A conversion failure returns the ORIGINAL body: a float array reaching a
// base64-expecting SDK is a corrupt vector, but an empty or half-rewritten body
// is a hard client error, and the caller still has the audit row either way.
func honorEmbeddingEncodingFormat(respBody []byte, metadata any) []byte {
	if requestedEmbeddingEncodingFormat(metadata) != "base64" {
		return respBody
	}
	if len(respBody) == 0 {
		return respBody
	}
	rows := gjson.GetBytes(respBody, "data")
	if !rows.IsArray() {
		return respBody
	}

	out := respBody
	converted := false
	var convErr bool
	rows.ForEach(func(idx, row gjson.Result) bool {
		emb := row.Get("embedding")
		switch {
		case emb.IsArray():
			encoded, ok := base64FromFloatArray(emb)
			if !ok {
				convErr = true
				return false
			}
			next, err := sjson.SetBytes(out, "data."+idx.String()+".embedding", encoded)
			if err != nil {
				convErr = true
				return false
			}
			out = next
			converted = true
			return true
		default:
			// Already a base64 string (or an unexpected scalar) — leave as-is.
			return true
		}
	})
	if convErr || !converted {
		return respBody
	}
	return out
}

// base64FromFloatArray packs a JSON number array into little-endian IEEE-754
// float32 bytes and base64-encodes them, matching OpenAI's wire encoding.
// Reports false when any element is not a number, so a malformed vector falls
// back to the untouched body rather than silently encoding zeros.
func base64FromFloatArray(arr gjson.Result) (string, bool) {
	values := arr.Array()
	buf := make([]byte, 0, len(values)*4)
	scratch := make([]byte, 4)
	for _, v := range values {
		if v.Type != gjson.Number {
			return "", false
		}
		binary.LittleEndian.PutUint32(scratch, math.Float32bits(float32(v.Float())))
		buf = append(buf, scratch...)
	}
	return base64.StdEncoding.EncodeToString(buf), true
}
