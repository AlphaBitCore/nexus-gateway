package codecs

import (
	"reflect"
	"strings"
	"testing"
)

// walkSSEFrames dispatches per FRAME, not per line (findings R-14 and R-15). These pin
// both halves, because both used to fail silently: a multi-line frame lost its Tier-1
// decode and the body fell through to Tier 3 verbatim, so hooks saw unstructured text
// instead of messages — under-scanning on a DLP path, with no error anywhere.

type sseCall struct {
	event string
	data  string
}

func collectFrames(t *testing.T, raw string) []sseCall {
	t.Helper()
	var got []sseCall
	if err := walkSSEFrames([]byte(raw), func(event, data string) {
		got = append(got, sseCall{event: event, data: data})
	}); err != nil {
		t.Fatalf("walkSSEFrames: %v", err)
	}
	return got
}

// TestWalkSSEFrames_JoinsMultiLineData is R-14. A W3C-legal frame carrying its payload
// across two data lines must arrive as ONE callback with the lines joined by "\n" — that
// is the only form in which the JSON parses.
func TestWalkSSEFrames_JoinsMultiLineData(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":\n" +
		"data: {\"content\":\"hi\"}}]}\n" +
		"\n"
	want := []sseCall{{data: "{\"choices\":[{\"delta\":\n{\"content\":\"hi\"}}]}"}}
	if got := collectFrames(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %#v, want %#v.\nPer-line dispatch would emit two JSON fragments, "+
			"neither of which parses, so the frame silently loses its Tier-1 decode.", got, want)
	}
}

// TestWalkSSEFrames_EventNameAppliesToWholeFrame is R-15. SSE collects a frame's fields
// and dispatches at the blank line, so an `event:` line AFTER the data line still names
// that frame. Binding the name only to following data lines reported event=="" here.
func TestWalkSSEFrames_EventNameAppliesToWholeFrame(t *testing.T) {
	raw := "data: {\"x\":1}\n" +
		"event: message_delta\n" +
		"\n"
	want := []sseCall{{event: "message_delta", data: "{\"x\":1}"}}
	if got := collectFrames(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %#v, want %#v (the event name must apply to the whole frame "+
			"regardless of field order)", got, want)
	}
}

// TestWalkSSEFrames_TrailingFrameWithoutBlankLine guards the regression the collect-then-
// dispatch rewrite could easily introduce: a capture that ends mid-stream has no final
// blank line, and dropping that frame would lose the last frame of every aborted stream —
// which is the normal shape for a client that disconnected.
func TestWalkSSEFrames_TrailingFrameWithoutBlankLine(t *testing.T) {
	raw := "data: {\"a\":1}\n\ndata: {\"b\":2}"
	want := []sseCall{{data: "{\"a\":1}"}, {data: "{\"b\":2}"}}
	if got := collectFrames(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %#v, want %#v — the unterminated trailing frame must still dispatch", got, want)
	}
}

// TestWalkSSEFrames_IgnoresCommentsAndOtherFields pins that the rewrite did not start
// treating spec-ignored lines as data. A `:` keepalive comment appearing inside a frame
// must not become part of the payload.
func TestWalkSSEFrames_IgnoresCommentsAndOtherFields(t *testing.T) {
	raw := ": keepalive\n" +
		"id: 42\n" +
		"retry: 3000\n" +
		"data: {\"a\":1}\n" +
		"\n"
	want := []sseCall{{data: "{\"a\":1}"}}
	if got := collectFrames(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %#v, want %#v — comments and id/retry are ignored per spec", got, want)
	}
}

// TestWalkSSEFrames_EventOnlyFrameDispatchesNothing preserves the previous behaviour for a
// keepalive frame carrying no data: it produced no callback then and must not produce an
// empty-payload one now, which every fold would count as a malformed frame against its
// coverage total.
func TestWalkSSEFrames_EventOnlyFrameDispatchesNothing(t *testing.T) {
	raw := "event: ping\n\ndata: {\"a\":1}\n\n"
	want := []sseCall{{data: "{\"a\":1}"}}
	if got := collectFrames(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %#v, want %#v — a data-less frame must dispatch nothing", got, want)
	}
}

// TestWalkSSEFrames_SingleLineFramesUnchanged is the no-regression assertion for the
// overwhelmingly common shape: one data line per frame, which must behave exactly as
// before.
func TestWalkSSEFrames_SingleLineFramesUnchanged(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"a":1}`, "",
		`data: {"b":2}`, "",
		`data: [DONE]`, "",
	}, "\n")
	want := []sseCall{{data: `{"a":1}`}, {data: `{"b":2}`}, {data: "[DONE]"}}
	if got := collectFrames(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %#v, want %#v", got, want)
	}
}
