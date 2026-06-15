package tlsbump

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestIsStreamingRequestBody(t *testing.T) {
	mk := func(method string, cl int64, hasBody bool) *http.Request {
		r := &http.Request{Method: method, ContentLength: cl}
		if hasBody {
			r.Body = io.NopCloser(strings.NewReader("x"))
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		want bool
	}{
		{"POST unknown-length -> stream", mk(http.MethodPost, -1, true), true},
		{"PUT unknown-length -> stream", mk(http.MethodPut, -1, true), true},
		{"PATCH unknown-length -> stream", mk(http.MethodPatch, -1, true), true},
		{"POST known-length -> buffer", mk(http.MethodPost, 128, true), false},
		{"POST zero-length -> buffer", mk(http.MethodPost, 0, true), false},
		{"GET unknown-length -> buffer", mk(http.MethodGet, -1, true), false},
		{"DELETE unknown-length -> buffer", mk(http.MethodDelete, -1, true), false},
		{"POST nil body -> buffer", mk(http.MethodPost, -1, false), false},
	}
	for _, c := range cases {
		if got := isStreamingRequestBody(c.r); got != c.want {
			t.Errorf("%s: isStreamingRequestBody = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBoundedCapture_CapsAndDropsOverflow(t *testing.T) {
	c := newBoundedCapture(5)
	// Write reports the FULL length (never short) so a TeeReader never fails the
	// underlying read, even when the capture is already full.
	n, err := c.Write([]byte("abc"))
	if n != 3 || err != nil {
		t.Fatalf("Write(abc) = %d,%v want 3,nil", n, err)
	}
	n, err = c.Write([]byte("defGH")) // only "de" fits (cap 5), rest dropped
	if n != 5 || err != nil {
		t.Fatalf("Write(defGH) = %d,%v want 5,nil (full length reported)", n, err)
	}
	if got := string(c.Bytes()); got != "abcde" {
		t.Fatalf("Bytes = %q, want abcde (capped at 5)", got)
	}
	if !c.Truncated() {
		t.Fatal("Truncated() = false, want true after overflow was dropped")
	}
	if fresh := newBoundedCapture(8); fresh.Truncated() {
		t.Fatal("fresh capture reports truncated")
	}
}

func TestBoundedCapture_BytesReturnsCopy(t *testing.T) {
	c := newBoundedCapture(16)
	_, _ = c.Write([]byte("hello"))
	b := c.Bytes()
	b[0] = 'H'
	if got := string(c.Bytes()); got != "hello" {
		t.Fatalf("Bytes returned an aliased slice: mutating it changed capture to %q", got)
	}
}

func TestBoundedCapture_ConcurrentWriteAndRead(t *testing.T) {
	c := newBoundedCapture(1 << 20)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 1000 {
			_, _ = c.Write([]byte("payload-chunk"))
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = c.Bytes() // must not race/panic
		}
	}()
	wg.Wait()
	if len(c.Bytes()) != 1000*len("payload-chunk") {
		t.Fatalf("captured %d bytes, want %d", len(c.Bytes()), 1000*len("payload-chunk"))
	}
}

// TestTeeReadCloser_StreamsAndCaptures proves reads pass through verbatim AND
// land in the capture incrementally (the streaming-relay invariant), and that
// Close closes the original body.
func TestTeeReadCloser_StreamsAndCaptures(t *testing.T) {
	cap := newBoundedCapture(1024)
	orig := &closeTracker{r: strings.NewReader("the-streamed-request-body")}
	body := &teeReadCloser{r: io.TeeReader(orig, cap), c: orig}

	// Read in small chunks to mimic the upstream transport draining the body.
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "the-streamed-request-body" {
		t.Fatalf("relayed body = %q, want the-streamed-request-body", got)
	}
	if string(cap.Bytes()) != "the-streamed-request-body" {
		t.Fatalf("capture = %q, want the-streamed-request-body", cap.Bytes())
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !orig.closed {
		t.Fatal("Close did not close the original body")
	}
}

func TestRequestBodyBytes_Resolution(t *testing.T) {
	cap := newBoundedCapture(64)
	_, _ = cap.Write([]byte("streamed"))

	cases := []struct {
		name string
		ac   *requestAuditCtx
		want string
	}{
		{"buffered eager body wins", &requestAuditCtx{requestBody: []byte("buffered"), requestCapture: cap, storeRequestBody: true}, "buffered"},
		{"streaming capture when storing", &requestAuditCtx{requestCapture: cap, storeRequestBody: true}, "streamed"},
		{"streaming capture not stored -> nil", &requestAuditCtx{requestCapture: cap, storeRequestBody: false}, ""},
		{"nothing -> nil", &requestAuditCtx{}, ""},
	}
	for _, c := range cases {
		if got := string(c.ac.requestBodyBytes()); got != c.want {
			t.Errorf("%s: requestBodyBytes = %q, want %q", c.name, got, c.want)
		}
	}
}

type closeTracker struct {
	r      io.Reader
	closed bool
}

func (c *closeTracker) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *closeTracker) Close() error               { c.closed = true; return nil }

// sanity: teeReadCloser satisfies io.ReadCloser
var _ io.ReadCloser = (*teeReadCloser)(nil)
