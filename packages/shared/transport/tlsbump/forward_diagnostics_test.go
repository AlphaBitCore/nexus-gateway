package tlsbump

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestIsStreamingContentType pins the streaming-vs-buffered routing fork: a
// Content-Type misclassified as non-streaming is buffered (io.ReadAll), which
// blocks the client until the whole stream ends and is the suspected cause of
// clients canceling long chat streams. Both the routing decision and the
// failure diagnostic call this, so the classification must be exact.
func TestIsStreamingContentType(t *testing.T) {
	streaming := []string{
		"text/event-stream",
		"text/event-stream; charset=utf-8",
		"application/connect+proto",
		"application/connect+json",
		"application/connect+json; charset=utf-8",
	}
	for _, ct := range streaming {
		if !isStreamingContentType(ct) {
			t.Errorf("isStreamingContentType(%q) = false, want true (would be buffered → stream breaks)", ct)
		}
	}
	buffered := []string{
		"",
		"application/json",
		"text/plain",
		"application/grpc", // gRPC (not connect) is not routed via this path
		"text/html",
	}
	for _, ct := range buffered {
		if isStreamingContentType(ct) {
			t.Errorf("isStreamingContentType(%q) = true, want false", ct)
		}
	}
}

// TestLooksLikeStreamingResponse covers the diagnostic smell heuristic:
// chunked transfer-encoding or an unknown Content-Length means "probably a
// stream" even when the Content-Type wasn't recognised — surfacing a
// mis-routed streaming reply in the logs.
func TestLooksLikeStreamingResponse(t *testing.T) {
	cases := []struct {
		name string
		resp *http.Response
		want bool
	}{
		{"nil", nil, false},
		{"chunked", &http.Response{TransferEncoding: []string{"chunked"}, ContentLength: -1}, true},
		{"no content-length", &http.Response{ContentLength: -1}, true},
		{"fixed length", &http.Response{ContentLength: 1024}, false},
		{"zero length", &http.Response{ContentLength: 0}, false},
	}
	for _, c := range cases {
		if got := looksLikeStreamingResponse(c.resp); got != c.want {
			t.Errorf("%s: looksLikeStreamingResponse = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResponseRouteName(t *testing.T) {
	if got := responseRouteName(true, nil); got != "sse-stream" {
		t.Errorf("isSSE → %q, want sse-stream", got)
	}
	if got := responseRouteName(false, nil); got != "unaudited-relay" {
		t.Errorf("audCtx nil → %q, want unaudited-relay (the silent-drop path)", got)
	}
	if got := responseRouteName(false, &requestAuditCtx{}); got != "buffered-or-fast" {
		t.Errorf("audCtx present → %q, want buffered-or-fast", got)
	}
}

func TestResponseArmName(t *testing.T) {
	if got := responseArmName(errors.New("boom"), false); got != "pipeline-build-error" {
		t.Errorf("pErr → %q, want pipeline-build-error", got)
	}
	if got := responseArmName(nil, true); got != "buffered-ai" {
		t.Errorf("needBuffer → %q, want buffered-ai", got)
	}
	if got := responseArmName(nil, false); got != "stream-through-fast" {
		t.Errorf("default → %q, want stream-through-fast", got)
	}
}

// TestCancelCause is the load-bearing failure-attribution helper: it lets the
// logs distinguish a CLIENT close (the client gave up / raced another
// connection) from OUR own deadline — the difference between "the client
// abandoned the stream" and "our proxy timed it out", which point at
// completely different fixes.
func TestCancelCause(t *testing.T) {
	live := context.Background()
	if got := cancelCause(live); got != "none" {
		t.Errorf("live ctx → %q, want none", got)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := cancelCause(canceled); got != "client_canceled" {
		t.Errorf("canceled ctx → %q, want client_canceled", got)
	}

	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	if got := cancelCause(expired); got != "our_deadline" {
		t.Errorf("deadline-exceeded ctx → %q, want our_deadline", got)
	}
}

// TestBodyLooksLikeEventStream is the guard for the distinction that made the
// buffered-stream diagnostic worth having at all.
//
// The pre-read heuristic it replaced (looksLikeStreamingResponse) answers "chunked or no
// Content-Length", which describes nearly every dynamically generated JSON response — it
// was true on 4 of 4 ordinary chat completions measured through the live proxy. A flag
// that is always set cannot distinguish the incident from the normal case, so raising it
// as an operator signal produced a WARN on every request.
//
// The two directions below are therefore not symmetric decoration. The JSON cases are the
// ones that matter: they are what the old heuristic got wrong, and a false positive here
// costs an operator a WARN per request.
func TestBodyLooksLikeEventStream(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"sse data frame", "data: {\"delta\":\"hi\"}\n\ndata: [DONE]\n\n", true},
		{"sse named event", "event: message\ndata: {}\n\n", true},
		{"sse retry directive", "retry: 3000\ndata: {}\n\n", true},
		{"sse after leading blank line", "\ndata: {}\n\n", true},
		{"sse with CRLF", "\r\ndata: {}\r\n\r\n", true},

		// A chat completion is the body this fires on in production if the check is
		// written loosely — note it CONTAINS the substring "data" nowhere, but the
		// quoted-key cases below do.
		{"ordinary chat completion json", `{"id":"chatcmpl-x","choices":[{"message":{"content":"ok"}}]}`, false},
		{"json with a quoted data key", `{"data":[{"id":"a"}],"object":"list"}`, false},
		{"json with a quoted event key", `{"event":"created","id":"evt_1"}`, false},
		{"json pretty-printed so the key starts a line", "{\n  \"data\": [1,2,3]\n}", false},
		{"empty body", "", false},
		{"plain text", "upstream error: bad gateway\n", false},

		// The window is a bound, not a scan of the whole body: a 100 KiB JSON document
		// that happens to contain an SSE-looking line far in must not be reclassified,
		// and a real stream always declares itself in its first frame.
		{"sse marker beyond the window", strings.Repeat("x", 400) + "\ndata: {}\n", false},
	}
	for _, c := range cases {
		if got := bodyLooksLikeEventStream([]byte(c.body)); got != c.want {
			t.Errorf("%s: bodyLooksLikeEventStream = %v, want %v (body %.60q)",
				c.name, got, c.want, c.body)
		}
	}
}
