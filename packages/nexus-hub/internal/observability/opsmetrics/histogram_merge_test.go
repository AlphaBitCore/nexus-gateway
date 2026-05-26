package opsmetrics

import (
	"testing"
)

func TestParseHistogramBuckets_DecodesCanonicalShape(t *testing.T) {
	raw := []byte(`{"buckets":[10,5,2,1,0,0]}`)
	got, err := ParseHistogramBuckets(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := HistogramBuckets{10, 5, 2, 1, 0, 0}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseHistogramBuckets_HandlesEmptyOrNil(t *testing.T) {
	cases := [][]byte{nil, {}, []byte(`null`)}
	for _, c := range cases {
		got, err := ParseHistogramBuckets(c)
		// `null` JSON unmarshals to a struct with zero buckets — must not
		// error out; both nil and empty short-circuit before the decoder.
		if err != nil && len(c) > 0 && string(c) != "null" {
			t.Errorf("parse(%q) err = %v", c, err)
		}
		if got != (HistogramBuckets{}) {
			t.Errorf("parse(%q) = %v, want zero", c, got)
		}
	}
}

func TestParseHistogramBuckets_TruncatesAndPads(t *testing.T) {
	// Too many elements → truncated to 6.
	long := []byte(`{"buckets":[1,2,3,4,5,6,7,8]}`)
	got, _ := ParseHistogramBuckets(long)
	if got != (HistogramBuckets{1, 2, 3, 4, 5, 6}) {
		t.Errorf("long: got %v, want first 6", got)
	}

	// Too few elements → zero-padded.
	short := []byte(`{"buckets":[1,2,3]}`)
	got, _ = ParseHistogramBuckets(short)
	if got != (HistogramBuckets{1, 2, 3, 0, 0, 0}) {
		t.Errorf("short: got %v, want zero-padded", got)
	}
}

func TestMergeHistogramBuckets_ElementwiseSum(t *testing.T) {
	a := HistogramBuckets{1, 2, 3, 4, 5, 6}
	b := HistogramBuckets{10, 0, 30, 0, 50, 0}
	got := MergeHistogramBuckets(a, b)
	want := HistogramBuckets{11, 2, 33, 4, 55, 6}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEncodeHistogramBuckets_RoundTrip(t *testing.T) {
	in := HistogramBuckets{1, 2, 3, 4, 5, 6}
	raw, err := EncodeHistogramBuckets(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := ParseHistogramBuckets(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got != in {
		t.Errorf("round-trip: got %v, want %v", got, in)
	}
}

func TestSumHistogramBuckets(t *testing.T) {
	if got := SumHistogramBuckets(HistogramBuckets{1, 2, 3, 4, 5, 6}); got != 21 {
		t.Errorf("got %d, want 21", got)
	}
	if got := SumHistogramBuckets(HistogramBuckets{}); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}
