package format

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// teeLikeWriter reproduces the condition that made the constants allocate: a
// wrapper that embeds the http.ResponseWriter INTERFACE, exactly as the stream
// capture tee does. The interface does not declare WriteString, so nothing is
// promoted and the wrapper does not satisfy io.StringWriter — which is what made
// io.WriteString fall back to w.Write([]byte(s)) and allocate a fresh slice for
// a constant on every SSE frame.
//
// Testing against a bytes.Buffer instead would prove nothing: a Buffer has
// WriteString, takes the fast path, and would have shown zero allocations while
// production paid on every frame.
type teeLikeWriter struct {
	http.ResponseWriter
	sink *bytes.Buffer
}

func (t *teeLikeWriter) Write(p []byte) (int, error) { return t.sink.Write(p) }

func newTeeLike() *teeLikeWriter {
	return &teeLikeWriter{sink: &bytes.Buffer{}}
}

// Guard the premise before the measurement: if this wrapper ever does satisfy
// io.StringWriter, the allocation assertions below stop testing anything.
func TestTeeLikeWriterIsNotAStringWriter(t *testing.T) {
	var w io.Writer = newTeeLike()
	if _, ok := w.(io.StringWriter); ok {
		t.Fatal("the tee-like wrapper now satisfies io.StringWriter, so it no longer " +
			"reproduces the shape that made the constants allocate — the assertions " +
			"below would pass for the wrong reason")
	}
}

func TestFrameConstantsDoNotAllocatePerFrame(t *testing.T) {
	cases := []struct {
		name  string
		write func(io.Writer) error
	}{
		{"frame terminator", func(w io.Writer) error { return WriteEvent(w, `{"a":1}`) }},
		{"done marker", WriteDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTeeLike()
			got := testing.AllocsPerRun(200, func() {
				w.sink.Reset()
				if err := tc.write(w); err != nil {
					t.Fatalf("write: %v", err)
				}
			})
			// WriteEvent assembles its line in a pooled buffer, so the whole
			// call should reach the wire without a heap allocation of its own.
			if got > 0 {
				t.Errorf("%s allocated %.2f times per write through an interface-embedding "+
					"wrapper; the constants are meant to reach Write as pre-made bytes",
					tc.name, got)
			}
		})
	}
}

// The bytes still have to be right. An optimisation that stops allocating and
// starts writing the wrong terminator would pass the assertion above.
func TestFrameConstantsStillWriteTheSameBytes(t *testing.T) {
	w := newTeeLike()
	if err := WriteEvent(w, `{"a":1}`); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if got, want := w.sink.String(), "data: {\"a\":1}\n\n"; got != want {
		t.Errorf("WriteEvent wrote %q, want %q", got, want)
	}
	w.sink.Reset()
	if err := WriteDone(w); err != nil {
		t.Fatalf("WriteDone: %v", err)
	}
	if got, want := w.sink.String(), "data: [DONE]\n\n"; got != want {
		t.Errorf("WriteDone wrote %q, want %q", got, want)
	}
}
