package bufconn

import (
	"errors"
	"io"
	"net"
	"testing"
)

func TestNew_EmptyPrefix_ReturnsConnUnchanged(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	if got := New(c2, nil); got != c2 {
		t.Fatal("empty prefix must return the underlying conn unchanged")
	}
	if got := New(c2, []byte{}); got != c2 {
		t.Fatal("zero-length prefix must return the underlying conn unchanged")
	}
}

func TestConn_Read_PrefixThenUnderlying(t *testing.T) {
	// The wrapper must yield the pre-buffered prefix first, then the bytes
	// off the real connection — mirroring the post-Hijack / post-SNI-peek
	// replay the proxy data path depends on.
	c1, c2 := net.Pipe()
	defer c1.Close()

	wrapped := New(c2, []byte("PRE"))
	if wrapped == c2 {
		t.Fatal("non-empty prefix must wrap the conn")
	}

	go func() {
		_, _ = c1.Write([]byte("BODY"))
		_ = c1.Close() // EOF terminates the wrapped read
	}()

	got, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "PREBODY" {
		t.Fatalf("got %q, want %q (prefix must precede the connection bytes)", got, "PREBODY")
	}
}

func TestBytesReader(t *testing.T) {
	r := &bytesReader{data: []byte("hello")}
	// Partial read.
	buf := make([]byte, 3)
	n, err := r.Read(buf)
	if err != nil || n != 3 || string(buf) != "hel" {
		t.Fatalf("first read: n=%d err=%v buf=%q", n, err, buf)
	}
	// Remainder.
	n, err = r.Read(buf)
	if err != nil || n != 2 || string(buf[:n]) != "lo" {
		t.Fatalf("second read: n=%d err=%v buf=%q", n, err, buf[:n])
	}
	// Exhausted → EOF.
	if n, err := r.Read(buf); !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("exhausted read: n=%d err=%v, want 0/EOF", n, err)
	}
}
