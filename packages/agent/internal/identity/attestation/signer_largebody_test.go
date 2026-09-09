package attestation

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"testing"
)

// InjectInto buffers the request body to hash it, bounded at 8 MiB. Past that
// bound it signs the empty-body hash and rejoins the buffered prefix with the
// rest of the original stream — except it had already CLOSED the original, so
// the MultiReader's second source was a closed body. Every upload over 8 MiB
// reached the upstream truncated to its first 8 MiB or failed outright, on a
// fail-open path whose entire purpose is not to disturb the request.
//
// These arms read the body back the way an http.Transport would.

// countingCloser is a body whose Close is observable, so a test can tell
// "closed too early" apart from "closed at the right time".
type countingCloser struct {
	io.Reader
	closed int
}

func (c *countingCloser) Close() error { c.closed++; return nil }

// errAfterN reads n bytes and then fails, reproducing a truncated upload.
type errAfterN struct {
	data   []byte
	n      int
	off    int
	closed int
}

func (e *errAfterN) Read(p []byte) (int, error) {
	if e.off >= e.n {
		return 0, errors.New("connection reset")
	}
	n := copy(p, e.data[e.off:e.n])
	e.off += n
	return n, nil
}

func (e *errAfterN) Close() error { e.closed++; return nil }

func newRequestWithBody(t *testing.T, body io.ReadCloser, length int64) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Body = body
	req.ContentLength = length
	req.GetBody = nil
	return req
}

// TestInjectInto_BodyOverCap_ReachesTheWireIntact is the arm that reproduces
// the defect: 9 MiB in, 9 MiB out, byte-identical.
func TestInjectInto_BodyOverCap_ReachesTheWireIntact(t *testing.T) {
	const size = 9 * 1024 * 1024
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	want := sha256.Sum256(payload)

	src := &countingCloser{Reader: bytes.NewReader(payload)}
	req := newRequestWithBody(t, src, size)

	// A disabled signer still runs the whole body path and simply omits the
	// header, which is the fail-open contract — and it keeps this test about
	// the body rather than about keystore wiring.
	s := NewSigner(nil, "k", "agent-1", func() bool { return false }, newTestLogger())
	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto: %v", err)
	}

	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reading the rewrapped body failed: %v — the transport would have sent a broken request", err)
	}
	if len(got) != size {
		t.Fatalf("body reached the wire as %d bytes, want %d — the upload was truncated", len(got), size)
	}
	if sha256.Sum256(got) != want {
		t.Error("body bytes differ from what the client wrote")
	}

	// The original must not have been closed before the rejoin; closing it is
	// the transport's job once it has finished sending.
	if src.closed != 0 {
		t.Errorf("the original body was closed %d time(s) during InjectInto — that is what made the rejoined reader return nothing", src.closed)
	}
	// ...and closing the rewrapped body must still reach the original, or the
	// connection it reads from leaks.
	if err := req.Body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if src.closed != 1 {
		t.Errorf("closing the rewrapped body closed the original %d time(s), want 1 — a NopCloser would drop it", src.closed)
	}
}

// GetBody must not claim a body is replayable when what it would replay is the
// truncated prefix. A redirect or transport retry would otherwise send a body
// the client never wrote.
func TestInjectInto_BodyOverCap_DoesNotInstallATruncatingGetBody(t *testing.T) {
	const size = 9 * 1024 * 1024
	payload := bytes.Repeat([]byte("x"), size)
	req := newRequestWithBody(t, &countingCloser{Reader: bytes.NewReader(payload)}, size)

	s := NewSigner(nil, "k", "agent-1", func() bool { return false }, newTestLogger())
	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto: %v", err)
	}
	if req.GetBody == nil {
		return // correct: not replayable
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	replay, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading GetBody: %v", err)
	}
	if len(replay) != size {
		t.Errorf("GetBody replays %d bytes of a %d-byte body — a retry would send a body the client never wrote", len(replay), size)
	}
}

// The sibling: a body UNDER the cap is still buffered, hashed, and fully
// replayable. Without this, "never touch the body" would satisfy the arms above
// while removing the feature.
func TestInjectInto_BodyUnderCap_IsBufferedAndReplayable(t *testing.T) {
	payload := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	src := &countingCloser{Reader: bytes.NewReader(payload)}
	req := newRequestWithBody(t, src, int64(len(payload)))

	s := NewSigner(nil, "k", "agent-1", func() bool { return false }, newTestLogger())
	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto: %v", err)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("body = %q (err %v), want the original", got, err)
	}
	if req.GetBody == nil {
		t.Fatal("a fully buffered body must stay replayable")
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	replay, _ := io.ReadAll(rc)
	if !bytes.Equal(replay, payload) {
		t.Errorf("GetBody replayed %q, want the original", replay)
	}
	// Reading to EOF is what makes closing the original correct on this path.
	if src.closed != 1 {
		t.Errorf("fully-buffered body closed the original %d time(s), want 1", src.closed)
	}
}

// A body that fails mid-read must surface the real error to the transport, not
// a silently truncated request. Before the fix the original was closed here
// too, so what the transport got was neither the body nor the error.
func TestInjectInto_BodyReadError_SurfacesRatherThanTruncating(t *testing.T) {
	const size = 9 * 1024 * 1024
	src := &errAfterN{data: bytes.Repeat([]byte("y"), size), n: 1024}
	req := newRequestWithBody(t, src, size)

	s := NewSigner(nil, "k", "agent-1", func() bool { return false }, newTestLogger())
	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto must stay fail-open: %v", err)
	}
	if src.closed != 0 {
		t.Errorf("the failing body was closed %d time(s) inside InjectInto", src.closed)
	}
	_, err := io.ReadAll(req.Body)
	if err == nil {
		t.Error("a body that failed mid-read read back cleanly — the transport would send a truncated request and call it a success")
	}
}

// closeStopsReads models what a real network body does: once closed, reads
// fail. bytes.Reader keeps returning data after Close, so a stub built on it
// can only detect the early close STRUCTURALLY (via a counter) and cannot
// demonstrate the consequence. This one does.
type closeStopsReads struct {
	r      *bytes.Reader
	closed bool
}

func (c *closeStopsReads) Read(p []byte) (int, error) {
	if c.closed {
		return 0, errors.New("http: read on closed response body")
	}
	return c.r.Read(p)
}

func (c *closeStopsReads) Close() error { c.closed = true; return nil }

// TestInjectInto_BodyOverCap_TruncationIsReal is the consequence arm. With a
// body that behaves like the network one, closing before the rejoin does not
// merely look wrong — the upload arrives as its first 8 MiB and nothing else.
func TestInjectInto_BodyOverCap_TruncationIsReal(t *testing.T) {
	const size = 9 * 1024 * 1024
	payload := bytes.Repeat([]byte("z"), size)
	src := &closeStopsReads{r: bytes.NewReader(payload)}
	req := newRequestWithBody(t, src, size)

	s := NewSigner(nil, "k", "agent-1", func() bool { return false }, newTestLogger())
	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto: %v", err)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reading the rewrapped body: %v", err)
	}
	if len(got) != size {
		t.Errorf("the upload arrived as %d of %d bytes — this is the truncation the early Close caused", len(got), size)
	}
}
