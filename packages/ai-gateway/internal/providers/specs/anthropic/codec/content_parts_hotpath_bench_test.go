package codec

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Absolute-cost measurement for the content-part projection, which is where
// carryCacheControl runs once per part per request.
//
// carryCacheControl cannot be measured alone in a way that means anything: it
// is a gjson field probe on a part the projection has already materialised, so
// the number that matters is the projection's cost with the probe in it versus
// without. The without side is projectPartsNoCacheControl below — the same loop
// with the marker copy removed, i.e. the code this replaced. It lives here only.
//
// 50 parts is the stated case; 5 is measured too, because a long conversation
// pays this per part and a short one is what most requests are.

var (
	partsSink    []map[string]any
	partsSinkErr error
)

// projectPartsNoCacheControl is the projection without the marker copy — the
// "before" that the cache_control carry was added to.
func projectPartsNoCacheControl(content gjson.Result) []map[string]any {
	var parts []map[string]any
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": part.Get("text").String()})
		case "image_url":
			parts = append(parts, map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "url", "url": part.Get("image_url.url").String()},
			})
		}
		return true
	})
	return parts
}

// contentArray builds a canonical content array of n text parts, optionally
// carrying a cache_control marker on the last one (which is where a caller puts
// it — the marker applies to everything before it).
func contentArray(n int, withMarker bool, markerOnEvery bool) gjson.Result {
	var b strings.Builder
	b.WriteString("[")
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"text","text":"segment of the conversation carrying a realistic ` +
			`amount of prose so the part is not degenerate"`)
		last := i == n-1
		if withMarker && (markerOnEvery || last) {
			b.WriteString(`,"cache_control":{"type":"ephemeral"}`)
		}
		b.WriteString("}")
	}
	b.WriteString("]")
	return gjson.Parse(b.String())
}

func benchProject(b *testing.B, content gjson.Result, wantMarkers int) {
	got, err := openAIPartsToAnthropicContent(content)
	if err != nil {
		b.Fatalf("arm precondition: %v", err)
	}
	markers := 0
	for _, p := range got {
		if _, ok := p["cache_control"]; ok {
			markers++
		}
	}
	if markers != wantMarkers {
		b.Fatalf("arm precondition: carried %d marker(s), want %d — the arm is not measuring "+
			"the branch its name claims", markers, wantMarkers)
	}
	b.ReportAllocs()
	for b.Loop() {
		partsSink, partsSinkErr = openAIPartsToAnthropicContent(content)
	}
}

func BenchmarkContentParts_50_NoMarker(b *testing.B) {
	benchProject(b, contentArray(50, false, false), 0)
}
func BenchmarkContentParts_50_OneMarker(b *testing.B) {
	benchProject(b, contentArray(50, true, false), 1)
}
func BenchmarkContentParts_50_MarkerOnEvery(b *testing.B) {
	benchProject(b, contentArray(50, true, true), 50)
}
func BenchmarkContentParts_5_NoMarker(b *testing.B) {
	benchProject(b, contentArray(5, false, false), 0)
}
func BenchmarkContentParts_5_OneMarker(b *testing.B) {
	benchProject(b, contentArray(5, true, false), 1)
}

// The before/after pair: the same 50 parts, projected with and without the
// marker probe. The delta is carryCacheControl's whole cost.
func BenchmarkContentParts_50_NoMarker_WithoutCarry(b *testing.B) {
	content := contentArray(50, false, false)
	if got := projectPartsNoCacheControl(content); len(got) != 50 {
		b.Fatalf("legacy projection produced %d parts, want 50", len(got))
	}
	b.ReportAllocs()
	for b.Loop() {
		partsSink = projectPartsNoCacheControl(content)
	}
}

// The two projections must agree on everything except the marker, or the delta
// above is a measurement of two different loops rather than of the probe.
func TestProjectionsAgreeExceptOnTheMarker(t *testing.T) {
	content := contentArray(8, false, false)
	withCarry, err := openAIPartsToAnthropicContent(content)
	if err != nil {
		t.Fatalf("projection errored: %v", err)
	}
	without := projectPartsNoCacheControl(content)
	if len(withCarry) != len(without) {
		t.Fatalf("part counts differ: %d vs %d", len(withCarry), len(without))
	}
	for i := range withCarry {
		if withCarry[i]["type"] != without[i]["type"] || withCarry[i]["text"] != without[i]["text"] {
			t.Fatalf("part %d differs beyond the marker: %v vs %v", i, withCarry[i], without[i])
		}
	}
	// And the marker really is carried when present, so the "OneMarker" arm is
	// not silently identical to "NoMarker".
	marked, err := openAIPartsToAnthropicContent(contentArray(8, true, false))
	if err != nil {
		t.Fatalf("projection errored: %v", err)
	}
	if _, ok := marked[7]["cache_control"]; !ok {
		t.Error("the marker was not carried; the marker arms measure the same thing as NoMarker")
	}
}
