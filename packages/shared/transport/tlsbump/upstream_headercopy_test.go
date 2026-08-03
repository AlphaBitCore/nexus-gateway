package tlsbump

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Correctness gate for the header-copy fast path in copyResponse (finding C-6). The
// claim is "same headers on the wire, less work per header", so the test does not
// restate what the headers should be — it runs the ORIGINAL Add-per-value loop beside
// the current implementation and requires the resulting header maps to be identical,
// including for the non-canonical keys the fast path must decline to take.

// copyHeadersReference is verbatim the loop this replaced. Oracle only.
func copyHeadersReference(dst http.Header, src http.Header) {
	for key, values := range src {
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

func headerCorpus() []http.Header {
	return []http.Header{
		// The canonical shape a provider actually returns.
		{
			"Content-Type":         {"text/event-stream"},
			"Cache-Control":        {"no-cache"},
			"X-Request-Id":         {"req_abc"},
			"Openai-Processing-Ms": {"42"},
		},
		// Multi-value headers — the fast path assigns the whole slice at once.
		{"Set-Cookie": {"a=1", "b=2", "c=3"}, "Vary": {"Accept", "Origin"}},
		// Keys written through a RAW MAP INDEX, i.e. never canonicalized. These are
		// the class the guard exists for: production has no such writer today, but a
		// future hook could add one and the failure would otherwise be silent.
		{"x-lowercase-key": {"v"}},
		{"X-MIXED-case-KEY": {"v"}},
		{"weird_underscore": {"v"}},
		// Empty value list and empty-string value.
		{"X-Empty-Values": {}},
		{"X-Empty-String": {""}},
		// A header whose value looks like a header, to catch any re-parsing.
		{"X-Nested": {"Content-Type: text/plain"}},
		{},
	}
}

// TestCopyResponse_HeadersMatchReference is the primary C-6 correctness proof: the
// header map the client sees is identical to what the Add-per-value loop produced.
func TestCopyResponse_HeadersMatchReference(t *testing.T) {
	for i, src := range headerCorpus() {
		// Current implementation, driven through the real production function.
		rec := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     src.Clone(),
			Body:       io.NopCloser(strings.NewReader("body")),
		}
		if err := copyResponse(rec, resp, nil); err != nil {
			t.Fatalf("corpus %d: copyResponse: %v", i, err)
		}

		// Oracle: same hop-by-hop stripping, then the old copy loop.
		want := http.Header{}
		stripped := src.Clone()
		for _, line := range stripped.Values("Connection") {
			for _, name := range strings.Split(line, ",") {
				if n := strings.TrimSpace(name); n != "" {
					stripped.Del(n)
				}
			}
		}
		for _, h := range hopByHopHeaders {
			stripped.Del(h)
		}
		copyHeadersReference(want, stripped)

		got := rec.Header()
		if len(got) != len(want) {
			t.Errorf("corpus %d: %d header keys, want %d\n got  %v\n want %v", i, len(got), len(want), got, want)
			continue
		}
		for k, wv := range want {
			gv, ok := got[k]
			if !ok {
				t.Errorf("corpus %d: key %q missing; got %v", i, k, got)
				continue
			}
			if len(gv) != len(wv) {
				t.Errorf("corpus %d: key %q has %d values, want %d (%v vs %v)", i, k, len(gv), len(wv), gv, wv)
				continue
			}
			for j := range wv {
				if gv[j] != wv[j] {
					t.Errorf("corpus %d: key %q value %d = %q, want %q", i, k, j, gv[j], wv[j])
				}
			}
		}
	}
}

// TestCopyResponse_DuplicateSpellingsBothSurvive covers the case deliberately kept
// out of the exact-order corpus above: canonical and non-canonical spellings of the
// same header present at once.
//
// Order is asserted loosely here, and that is a property of the problem rather than a
// weakened test. Both spellings canonicalize to one key, so which value lands first
// depends on Go's map iteration order — in the ORIGINAL implementation just as much as
// in this one. Demanding a fixed order would have been asserting something neither
// version guarantees, which is what the exact-order comparison started out doing and
// why it failed intermittently. What must hold, and is asserted, is that no value is
// lost: the fast path assigning over a slot the slow path had already filled is a real
// bug, and it is the one this case exists to catch.
func TestCopyResponse_DuplicateSpellingsBothSurvive(t *testing.T) {
	for range 20 { // repeat: the clobber is map-order dependent, so one pass can miss it
		rec := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"X-Dup": {"canonical"}, "x-dup": {"raw"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}
		if err := copyResponse(rec, resp, nil); err != nil {
			t.Fatalf("copyResponse: %v", err)
		}
		got := rec.Header()["X-Dup"]
		if len(got) != 2 {
			t.Fatalf("X-Dup = %v, want both values present — one spelling clobbered the other", got)
		}
		var sawCanonical, sawRaw bool
		for _, v := range got {
			switch v {
			case "canonical":
				sawCanonical = true
			case "raw":
				sawRaw = true
			}
		}
		if !sawCanonical || !sawRaw {
			t.Fatalf("X-Dup = %v, want both \"canonical\" and \"raw\"", got)
		}
	}
}

// TestCopyResponse_PreExistingHeaderOnWriterIsNotClobbered is the production shape of
// the same hazard: a stage before copyResponse already set the header on w. Assigning
// the upstream's slice directly would drop what was there, where Add appended to it.
func TestCopyResponse_PreExistingHeaderOnWriterIsNotClobbered(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Nexus-Via", "compliance-proxy")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Nexus-Via": {"upstream-added"}, "Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := copyResponse(rec, resp, nil); err != nil {
		t.Fatalf("copyResponse: %v", err)
	}
	got := rec.Header()["X-Nexus-Via"]
	if len(got) != 2 || got[0] != "compliance-proxy" {
		t.Errorf("X-Nexus-Via = %v, want the pre-existing value kept first and the upstream's appended", got)
	}
}

// TestCopyResponse_NonCanonicalKeyTakesTheSlowPath pins the guard itself. A
// non-canonical key must be normalized on the way out exactly as Add would have done
// — if the fast path ever swallowed it, the key would reach the client verbatim and
// a client looking it up by its canonical name would not find it.
func TestCopyResponse_NonCanonicalKeyTakesTheSlowPath(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"x-nexus-lowercase": {"present"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := copyResponse(rec, resp, nil); err != nil {
		t.Fatalf("copyResponse: %v", err)
	}
	if got := rec.Header().Get("X-Nexus-Lowercase"); got != "present" {
		t.Errorf("canonical lookup found %q — the non-canonical key was not normalized on copy; header map is %v", got, rec.Header())
	}
	//nolint:staticcheck // SA1008 flags the non-canonical key, which is exactly what
	// this asserts is absent: a canonical lookup here would find the correctly
	// normalized entry and the test would pass while proving nothing.
	if _, raw := rec.Header()["x-nexus-lowercase"]; raw {
		t.Errorf("the non-canonical key survived verbatim into the client header map: %v", rec.Header())
	}
}

// TestCopyResponse_HopByHopStillStripped guards the behaviour the fast path sits
// next to: a faster copy is worthless if it starts forwarding connection-scoped
// headers to the client.
func TestCopyResponse_HopByHopStillStripped(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Connection":        {"keep-alive, X-Custom-Hop"},
			"Keep-Alive":        {"timeout=5"},
			"Transfer-Encoding": {"chunked"},
			"X-Custom-Hop":      {"must-be-dropped"},
			"Content-Type":      {"application/json"},
		},
		Body: io.NopCloser(strings.NewReader("")),
	}
	if err := copyResponse(rec, resp, nil); err != nil {
		t.Fatalf("copyResponse: %v", err)
	}
	for _, gone := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "X-Custom-Hop"} {
		if v := rec.Header().Get(gone); v != "" {
			t.Errorf("hop-by-hop header %q forwarded to the client with value %q", gone, v)
		}
	}
	if v := rec.Header().Get("Content-Type"); v != "application/json" {
		t.Errorf("end-to-end header dropped: Content-Type = %q", v)
	}
}
