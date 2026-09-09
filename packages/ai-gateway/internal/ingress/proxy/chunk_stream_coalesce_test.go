package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	streamcache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// coalesceFrames builds n SSE frames plus a terminal one, in the shape a stream
// decoder produces: RawBytes carrying a complete frame.
func coalesceFrames(n int) []provcore.Chunk {
	out := make([]provcore.Chunk, 0, n+1)
	for i := range n {
		out = append(out, provcore.Chunk{RawBytes: fmt.Appendf(nil, "data: {\"i\":%d}\n\n", i)})
	}
	out = append(out, provcore.Chunk{Done: true, RawBytes: []byte("data: [DONE]\n\n")})
	return out
}

// readySub hands every chunk over without waiting — the shape of a cached
// replay, where the whole timeline is already in memory.
type readySub struct {
	chunks []provcore.Chunk
	idx    int
}

// Next checks the context first, exactly as replaySub.Next does. Without that
// this fake reports EOF where the real subscription reports the cancellation,
// which turns "the reader lost the client-abort signal" into "the stream ended
// normally" — the very confusion a test about cancellation exists to catch.
func (s *readySub) Next(ctx context.Context) (provcore.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return provcore.Chunk{}, err
	}
	if s.idx >= len(s.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	c := s.chunks[s.idx]
	s.idx++
	return c, nil
}
func (s *readySub) TryNext() (provcore.Chunk, bool) {
	if s.idx >= len(s.chunks) {
		return provcore.Chunk{}, false
	}
	c := s.chunks[s.idx]
	s.idx++
	return c, true
}
func (s *readySub) Close() error { return nil }

// liveSub hands over one chunk per blocking call and never has one ready ahead
// of the reader — the shape of a live upstream, where the reader is always in
// front of the model.
type liveSub struct {
	chunks []provcore.Chunk
	idx    int
}

func (s *liveSub) Next(context.Context) (provcore.Chunk, error) {
	if s.idx >= len(s.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	c := s.chunks[s.idx]
	s.idx++
	return c, nil
}
func (s *liveSub) TryNext() (provcore.Chunk, bool) { return provcore.Chunk{}, false }
func (s *liveSub) Close() error                    { return nil }

// plainSub implements ChunkSubscription and NOTHING else — no TryNext at all —
// so the reader's type assertion must fail on it. Written standalone rather than
// by embedding liveSub: an embedded TryNext that answers "not ready" makes the
// assertion's outcome unobservable, and a test that cannot see the difference
// cannot test it.
type plainSub struct {
	chunks []provcore.Chunk
	idx    int
}

func (s *plainSub) Next(context.Context) (provcore.Chunk, error) {
	if s.idx >= len(s.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	c := s.chunks[s.idx]
	s.idx++
	return c, nil
}
func (s *plainSub) Close() error { return nil }

// cancelOnFirstNext hands over the first chunk and cancels the request context on
// the way out, standing in for a client that hangs up just as its opening frame
// is relayed. Collection runs immediately after that Next returns, so this places
// the disconnect exactly where the reader has to notice it.
type cancelOnFirstNext struct {
	readySub
	cancel context.CancelFunc
	fired  bool
}

func (s *cancelOnFirstNext) Next(ctx context.Context) (provcore.Chunk, error) {
	c, err := s.readySub.Next(ctx)
	if !s.fired {
		s.fired = true
		s.cancel()
	}
	return c, err
}

// drain reads the whole stream and reports the bytes plus how many Reads
// returned data. The pump above this reader does one Write and one Flush per
// Read that returns bytes, so the count IS the write count.
func drain(t *testing.T, r *chunkSSEReader) (payload []byte, writes int) {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 32<<10)
	for range 10000 {
		n, err := r.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			writes++
		}
		if errors.Is(err, io.EOF) {
			return out.Bytes(), writes
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	t.Fatal("stream did not terminate")
	return nil, 0
}

func newTestReader(sub streamcache.ChunkSubscription) *chunkSSEReader {
	r := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI, false)
	r.usageSink = &chunkUsageHolder{}
	return r
}

// A cached response is a slice already in memory, so relaying it one frame per
// write spends a syscall per recorded frame on content that finished being
// produced long ago.
func TestCoalesce_ReplayCollapsesIntoOneWrite(t *testing.T) {
	const n = 40
	got, writes := drain(t, newTestReader(&readySub{chunks: coalesceFrames(n)}))

	if writes != 1 {
		t.Errorf("writes = %d for a %d-frame cached response; the whole timeline was "+
			"available from the first Read", writes, n+1)
	}
	want, _ := drain(t, newTestReader(&liveSub{chunks: coalesceFrames(n)}))
	if !bytes.Equal(got, want) {
		t.Fatalf("coalesced payload differs from the frame-at-a-time payload:\n got %q\nwant %q",
			got, want)
	}
}

// The half that must not move. Batching is only ever allowed to collect what the
// caller would have found on its next trip; the moment it changes how a live
// stream reaches the client, it has started trading the product's defining
// property for syscalls.
func TestCoalesce_LiveStreamIsUnchangedFrameForFrame(t *testing.T) {
	const n = 40
	frames := coalesceFrames(n)
	got, writes := drain(t, newTestReader(&liveSub{chunks: frames}))

	if writes != len(frames) {
		t.Fatalf("writes = %d for %d live frames — a stream whose next chunk is never "+
			"ready must still leave the reader one frame per write", writes, len(frames))
	}
	// Write count alone is not "frame for frame": a change that mangled a frame
	// while preserving the count would pass. Compare the bytes against the
	// coalesced arm, which carries the same timeline.
	want, _ := drain(t, newTestReader(&readySub{chunks: coalesceFrames(n)}))
	if !bytes.Equal(got, want) {
		t.Fatalf("live payload differs from the coalesced payload:\n got %q\nwant %q", got, want)
	}
}

// A subscription that does not implement ReadySubscription must take the
// pre-change path, so adding the optional half cannot change any existing
// caller.
//
// The assertion has to be able to SEE the type assertion decide, which means
// contrasting it with a subscription holding the SAME timeline that DOES
// implement the interface. Asserting only "the plain one writes N times" is
// green whether the assertion succeeds or fails, because a not-ready answer and
// a failed assertion leave coalesceReady at the same early return.
func TestCoalesce_SubscriptionWithoutTheOptionalHalfIsUntouched(t *testing.T) {
	const n = 12
	frames := coalesceFrames(n)

	plainBytes, plainWrites := drain(t, newTestReader(&plainSub{chunks: frames}))
	readyBytes, readyWrites := drain(t, newTestReader(&readySub{chunks: coalesceFrames(n)}))

	if plainWrites != len(frames) {
		t.Errorf("plain subscription: writes = %d, want %d — it must relay one frame "+
			"per write exactly as before the optional half existed", plainWrites, len(frames))
	}
	if readyWrites != 1 {
		t.Errorf("ready subscription: writes = %d, want 1 — without this the comparison "+
			"below proves nothing, because both arms would be taking the same path",
			readyWrites)
	}
	if !bytes.Equal(plainBytes, readyBytes) {
		t.Fatalf("the two paths delivered different bytes:\nplain %q\nready %q",
			plainBytes, readyBytes)
	}
}

// The Done chunk ends the stream wherever it lands. Collecting past it would
// relay frames the terminal frame already closed.
func TestCoalesce_StopsAtTheTerminalFrame(t *testing.T) {
	chunks := []provcore.Chunk{
		{RawBytes: []byte("data: {\"i\":0}\n\n")},
		{Done: true, RawBytes: []byte("data: [DONE]\n\n")},
		{RawBytes: []byte("data: {\"after\":\"done\"}\n\n")},
	}
	got, _ := drain(t, newTestReader(&readySub{chunks: chunks}))

	if bytes.Contains(got, []byte("after")) {
		t.Fatalf("relayed a frame recorded after the terminal one: %q", got)
	}
	if !bytes.Contains(got, []byte("[DONE]")) {
		t.Fatalf("terminal frame missing from %q", got)
	}
}

// failAfter transcodes n chunks and then fails, so a coalescing Read hits an
// encode error with valid frames already collected.
type failAfter struct {
	n   int
	err error
}

func (f *failAfter) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	if f.n <= 0 {
		return nil, f.err
	}
	f.n--
	return fmt.Appendf(nil, "data: {\"d\":%q}\n\n", chunk.Delta), nil
}

// A failure partway through collection must not swallow the frames already
// collected. Discarding them to report the error immediately would drop content
// the un-coalesced path had already delivered by that point — the client would
// see a shorter stream on a cache hit than on a miss of the same response.
func TestCoalesce_EncodeFailureKeepsTheFramesAlreadyCollected(t *testing.T) {
	boom := errors.New("transcoder refused the chunk")
	chunks := []provcore.Chunk{
		{Delta: "one"}, {Delta: "two"}, {Delta: "three"},
		{Done: true, RawBytes: []byte("data: [DONE]\n\n")},
	}
	r := newChunkSSEReaderFromSubscription(context.Background(),
		&readySub{chunks: chunks}, &failAfter{n: 2, err: boom}, provcore.FormatAnthropic, false)
	r.usageSink = &chunkUsageHolder{}

	buf := make([]byte, 32<<10)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("first Read returned %v; the frames encoded before the failure are "+
			"valid and must go out", err)
	}
	if got := string(buf[:n]); !bytes.Contains([]byte(got), []byte("one")) ||
		!bytes.Contains([]byte(got), []byte("two")) {
		t.Fatalf("payload %q is missing a frame that encoded cleanly", got)
	}

	// The failure surfaces on the next Read, which is the same order the
	// un-coalesced path produces: previous frame out, then the error.
	if _, err = r.Read(buf); !errors.Is(err, boom) {
		t.Fatalf("second Read returned %v, want the encode failure", err)
	}
	if te := r.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Fatalf("terminal error not stamped for the audit row: %+v", te)
	}
}

// A client that hangs up must end the stream at the cancellation, not at
// whatever the coalescing loop would have reached.
//
// The audit consequence is why this is not merely wasted work. CLIENT_ABORT is
// stamped by the blocking Next this reader returns to; a Read that drains a
// cached replay all the way to Done never goes back, so the row for an aborted
// cache hit would be filed as a clean completion — and this reader is the only
// producer of that code on this lane. "TryNext never waits, so there is nothing
// to cancel" is true of one call and false of a loop over them.
func TestCoalesce_CancelledClientStopsCollectionAndStillStampsAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// The client hangs up as the first frame goes out — i.e. between the blocking
	// Next that produced it and the collection that would follow. Cancelling
	// after the first Read RETURNS would be too late to test anything: by then
	// the collection has already run.
	sub := &cancelOnFirstNext{readySub: readySub{chunks: coalesceFrames(20)}, cancel: cancel}
	r := newChunkSSEReaderFromSubscription(ctx, sub, nil, provcore.FormatOpenAI, false)
	r.usageSink = &chunkUsageHolder{}

	buf := make([]byte, 32<<10)
	n, err := r.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("first Read: n=%d err=%v", n, err)
	}
	first := string(buf[:n])

	// Everything after the cancellation is the reader's own doing, so it must
	// stop rather than run the replay out.
	if _, err = r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("second Read returned %v, want context.Canceled — the reader kept "+
			"draining an in-memory replay for a client that had already hung up", err)
	}
	te := r.terminalError()
	if te == nil || te.code != streamErrCodeClientAbort {
		t.Fatalf("terminal error = %+v, want %s — an aborted replay filed as a clean "+
			"completion is the audit row losing the only signal that says the client left",
			te, streamErrCodeClientAbort)
	}
	// Exactly one frame, not merely "not the whole replay". The two guards fail
	// differently and only this distinguishes them: without the per-iteration
	// check the reader runs to Done (caught above), while without the entry check
	// it collects one extra frame before the loop notices — visible here and
	// nowhere else. A cancelled client should get neither.
	if got := strings.Count(first, "data: "); got != 1 {
		t.Fatalf("first Read delivered %d frames (%q); a client that hung up before "+
			"collection began should have cost exactly the one frame already encoded",
			got, first)
	}
}

// The cap bounds one Read, not the stream: everything still arrives, just across
// more than one write.
func TestCoalesce_CapBoundsTheReadNotTheStream(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 8<<10)
	chunks := make([]provcore.Chunk, 0, 33)
	for range 32 {
		chunks = append(chunks, provcore.Chunk{RawBytes: append(append([]byte("data: "), big...), '\n', '\n')})
	}
	chunks = append(chunks, provcore.Chunk{Done: true, RawBytes: []byte("data: [DONE]\n\n")})

	got, writes := drain(t, newTestReader(&readySub{chunks: chunks}))

	if writes < 2 {
		t.Fatalf("writes = %d: 256KB of frames went out under a %dB cap", writes, coalesceMaxBytes)
	}
	if n := bytes.Count(got, big); n != 32 {
		t.Fatalf("relayed %d of 32 large frames — the cap dropped content instead of "+
			"splitting the write", n)
	}
	if !bytes.Contains(got, []byte("[DONE]")) {
		t.Fatal("terminal frame missing")
	}
}
