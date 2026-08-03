package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The sizing algorithm and its properties are tested in shared/transport/bodyread, where the
// algorithm now lives. What remains testable HERE is the wiring — and a wrapper this small
// still has exactly two ways to be wrong, both silent:
//
//   - forwarding a wrong declared length (0 or -1 instead of resp.ContentLength), which loses
//     the finding's entire win while every test that only checks the body still passes;
//   - transposing declaredLen and maxBytes, which either truncates a good body or removes the
//     cap.
//
// Both are asserted below. Nothing else about the algorithm is re-asserted here.

func respWith(body string, contentLength int64) *http.Response {
	return &http.Response{
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: contentLength,
	}
}

// TestReadResponseBounded_ForwardsContentLength pins the win itself. An honestly-declared
// body must land in one exact allocation; if the wrapper passed 0 or -1 as the declared
// length, geometric growth would run instead and the body would still be correct — so this is
// asserted on capacity, which is the only observable difference.
func TestReadResponseBounded_ForwardsContentLength(t *testing.T) {
	body := strings.Repeat("z", 8<<10)
	got, err := readResponseBounded(respWith(body, int64(len(body))), 32<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != body {
		t.Fatalf("body length = %d, want %d", len(got), len(body))
	}
	if cap(got) > len(body)+1 {
		t.Fatalf("capacity = %d for an honestly-declared %d-byte body, want at most %d — "+
			"resp.ContentLength did not reach bodyread.Bounded, so the sizing win is gone while "+
			"the body still looks right",
			cap(got), len(body), len(body)+1)
	}
}

// TestReadResponseBounded_CapIsTheSecondArgument catches a transposed declaredLen/maxBytes.
// With the arguments the right way round a 500-byte chunked body truncates to 100; transposed,
// maxBytes would be -1 and the read would return nothing at all.
func TestReadResponseBounded_CapIsTheSecondArgument(t *testing.T) {
	body := strings.Repeat("x", 500)
	got, err := readResponseBounded(respWith(body, -1), 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100 — the cap must bound the read; getting 0 here means the "+
			"declared length and the cap were passed in the wrong order", len(got))
	}
}

func TestReadResponseBounded_NilAndEmpty(t *testing.T) {
	if got, err := readResponseBounded(nil, 1<<20); got != nil || err != nil {
		t.Fatalf("nil response = (%v, %v), want (nil, nil)", got, err)
	}
	if got, err := readResponseBounded(&http.Response{ContentLength: 5}, 1<<20); got != nil || err != nil {
		t.Fatalf("nil body = (%v, %v), want (nil, nil) — a response with no body must not be "+
			"dereferenced", got, err)
	}
	got, err := readResponseBounded(respWith("", 0), 1<<20)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty body = (%q, %v), want (empty, nil)", got, err)
	}
}
