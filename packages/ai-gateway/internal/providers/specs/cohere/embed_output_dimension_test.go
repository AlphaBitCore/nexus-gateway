package cohere

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// Cohere's embedding wire, probed from the prod host on 2026-08-19 against
// api.cohere.com/v2/embed:
//
//	embed-v4.0          output_dimension 256/512/1024/1536 → 200, vector is
//	                    exactly that wide; 128/384/768/999/2048/3072/4096 →
//	                    422 "N is not a valid output_dimension value for this
//	                    model, [256 512 1024 1536] is supported"; omitted → 1536
//	embed-english-v3.0  output_dimension present at all → 400 "invalid request:
//	                    output_dimension…"; omitted → 1024
//
// So v4 accepts a fixed SET of widths — not a range, which is why its catalog
// row enumerates rather than declaring min/max — and v3 accepts no such field.
// The codec had been dropping `dimensions` for every Cohere model on the
// stated grounds that they are all fixed-dimension. That was true when it was
// written and went stale with v4: a caller asking embed-v4.0 for 512 got HTTP
// 200 and a 1536-wide vector, which is not a clamped answer but a different
// object — one that silently corrupts whatever index it is written into.

func encodeEmbed(t *testing.T, canonical, providerModelID string) []byte {
	t.Helper()
	res, err := canonicalToCohereEmbed([]byte(canonical), provcore.CallTarget{ProviderModelID: providerModelID})
	if err != nil {
		t.Fatalf("canonicalToCohereEmbed: %v", err)
	}
	return res.Body
}

// TestEmbed_V4CarriesDimensionsAsOutputDimension — the wire can carry it, so
// dropping it is sending the wrong form, not honouring a limitation.
func TestEmbed_V4CarriesDimensionsAsOutputDimension(t *testing.T) {
	for _, dim := range []int64{256, 512, 1024, 1536} {
		body := encodeEmbed(t, `{"input":"hello","dimensions":`+itoa(dim)+`}`, "embed-v4.0")
		got := gjson.GetBytes(body, "output_dimension")
		if !got.Exists() {
			t.Fatalf("dimensions=%d must reach the wire as output_dimension; body=%s", dim, body)
		}
		if got.Int() != dim {
			t.Errorf("output_dimension = %d, want %d", got.Int(), dim)
		}
	}
}

// TestEmbed_V3NeverCarriesOutputDimension — v3 answers 400 to the field's mere
// presence, so the widening must be gated on the generation that accepts it.
func TestEmbed_V3NeverCarriesOutputDimension(t *testing.T) {
	for _, model := range []string{"embed-english-v3.0", "embed-multilingual-v3.0"} {
		body := encodeEmbed(t, `{"input":"hello","dimensions":512}`, model)
		if gjson.GetBytes(body, "output_dimension").Exists() {
			t.Errorf("%s rejects output_dimension outright; it must not be sent. body=%s", model, body)
		}
	}
}

// TestEmbed_NoDimensionsMeansNoField — omitting the field is how a caller asks
// for the model's default, and must stay that way on the wire.
func TestEmbed_NoDimensionsMeansNoField(t *testing.T) {
	body := encodeEmbed(t, `{"input":"hello"}`, "embed-v4.0")
	if gjson.GetBytes(body, "output_dimension").Exists() {
		t.Errorf("no dimensions requested → no output_dimension on the wire; body=%s", body)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
