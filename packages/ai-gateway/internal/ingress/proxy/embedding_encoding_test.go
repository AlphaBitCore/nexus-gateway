package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"strconv"
	"testing"

	"github.com/tidwall/gjson"
)

// metaWithEncoding builds the rec.Metadata shape preStampEmbeddingRequestMeta
// produces, carrying just the encoding_format the response path reads.
func metaWithEncoding(format string) any {
	return map[string]any{"embedding": map[string]any{"encoding_format": format}}
}

// decodeB64Floats reverses the wire encoding under test: little-endian IEEE-754
// float32, 4 bytes per component.
func decodeB64Floats(t *testing.T, s string) []float32 {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if len(raw)%4 != 0 {
		t.Fatalf("payload length %d is not a multiple of 4 — not packed float32", len(raw))
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}

func TestHonorEmbeddingEncodingFormat_ReEncodesFloatArrayToBase64(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1.5,-2.25,0]}]}`)

	got := honorEmbeddingEncodingFormat(body, metaWithEncoding("base64"))

	emb := gjson.GetBytes(got, "data.0.embedding")
	if emb.Type != gjson.String {
		t.Fatalf("embedding should be a base64 string, got %s", emb.Type)
	}
	floats := decodeB64Floats(t, emb.Str)
	want := []float32{1.5, -2.25, 0}
	if len(floats) != len(want) {
		t.Fatalf("decoded %d floats, want %d — a width mismatch here is the AP-3 corruption bug", len(floats), len(want))
	}
	for i := range want {
		if floats[i] != want[i] {
			t.Errorf("component %d = %v, want %v", i, floats[i], want[i])
		}
	}
	// Sibling fields must survive the rewrite.
	if gjson.GetBytes(got, "data.0.index").Int() != 0 || gjson.GetBytes(got, "object").String() != "list" {
		t.Errorf("rewrite clobbered surrounding fields: %s", got)
	}
}

func TestHonorEmbeddingEncodingFormat_PreservesVectorWidth(t *testing.T) {
	// The regression this whole file exists for: openai-node decodes the payload
	// as float32, so a 768-component vector MUST come back as 3072 bytes. The bug
	// shipped 768 bytes, which the SDK read as 192 components of garbage.
	const n = 768
	body := []byte(`{"data":[{"embedding":[` + repeatCSV("0.5", n) + `]}]}`)

	got := honorEmbeddingEncodingFormat(body, metaWithEncoding("base64"))

	raw, err := base64.StdEncoding.DecodeString(gjson.GetBytes(got, "data.0.embedding").Str)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != n*4 {
		t.Fatalf("payload is %d bytes for %d floats, want %d — the SDK would report %d components",
			len(raw), n, n*4, len(raw)/4)
	}
}

func TestHonorEmbeddingEncodingFormat_EncodesEveryBatchRow(t *testing.T) {
	body := []byte(`{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2]},{"index":2,"embedding":[3]}]}`)

	got := honorEmbeddingEncodingFormat(body, metaWithEncoding("base64"))

	for i := range 3 {
		emb := gjson.GetBytes(got, "data."+strconv.Itoa(i)+".embedding")
		if emb.Type != gjson.String {
			t.Fatalf("row %d not encoded: %s", i, emb.Raw)
		}
		if v := decodeB64Floats(t, emb.Str); len(v) != 1 || v[0] != float32(i+1) {
			t.Errorf("row %d decoded to %v, want [%d]", i, v, i+1)
		}
	}
}

func TestHonorEmbeddingEncodingFormat_NoOpCases(t *testing.T) {
	floatBody := []byte(`{"data":[{"embedding":[1,2,3]}]}`)
	alreadyB64 := []byte(`{"data":[{"embedding":"AACAPw=="}]}`)

	cases := []struct {
		name string
		body []byte
		meta any
	}{
		{"float requested", floatBody, metaWithEncoding("float")},
		{"no encoding stamped", floatBody, map[string]any{}},
		{"nil metadata", floatBody, nil},
		{"already base64", alreadyB64, metaWithEncoding("base64")},
		{"no data array", []byte(`{"error":{"message":"nope"}}`), metaWithEncoding("base64")},
		{"empty body", nil, metaWithEncoding("base64")},
		{"empty data array", []byte(`{"data":[]}`), metaWithEncoding("base64")},
		{"embedding is neither array nor string", []byte(`{"data":[{"embedding":42}]}`), metaWithEncoding("base64")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := honorEmbeddingEncodingFormat(tc.body, tc.meta)
			if string(got) != string(tc.body) {
				t.Errorf("body was rewritten but should have been left alone:\n got %s\nwant %s", got, tc.body)
			}
		})
	}
}

func TestHonorEmbeddingEncodingFormat_MalformedVectorLeavesBodyIntact(t *testing.T) {
	// A non-numeric component means we cannot pack the vector. Returning the
	// original body keeps the caller's data readable rather than shipping zeros.
	body := []byte(`{"data":[{"embedding":[1,"two",3]}]}`)

	got := honorEmbeddingEncodingFormat(body, metaWithEncoding("base64"))

	if string(got) != string(body) {
		t.Errorf("malformed vector should leave the body untouched, got %s", got)
	}
}

func TestRequestedEmbeddingEncodingFormat(t *testing.T) {
	cases := []struct {
		name string
		meta any
		want string
	}{
		{"base64", metaWithEncoding("base64"), "base64"},
		{"float", metaWithEncoding("float"), "float"},
		{"no embedding submap", map[string]any{"other": 1}, ""},
		{"not a map", "nonsense", ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestedEmbeddingEncodingFormat(tc.meta); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func repeatCSV(v string, n int) string {
	out := make([]byte, 0, n*(len(v)+1))
	for i := range n {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, v...)
	}
	return string(out)
}
