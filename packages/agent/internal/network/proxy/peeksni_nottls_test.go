package proxy

import (
	"errors"
	"net"
	"testing"
	"time"
)

// TestPeekSNI_PlainHTTPReturnsFast pins the fix for the plain-HTTP stall: a
// non-TLS first byte (here an HTTP request) must make PeekSNI return
// immediately with ErrNotTLSClientHello and the 5 peeked bytes for replay —
// NOT parse header[3:5] as a record length and block reading a body that
// never arrives (the old behaviour: a ~5 s stall + a corrupted/broken
// plain-HTTP flow once the agent is in the path).
func TestPeekSNI_PlainHTTPReturnsFast(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		// Client speaks first with plaintext HTTP — not a TLS ClientHello.
		_, _ = client.Write([]byte("GET /v1/chat HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	}()

	done := make(chan struct{})
	var sni string
	var peeked []byte
	var err error
	go func() {
		// Generous deadline; the point is PeekSNI must return in ~ms, not wait.
		sni, peeked, err = PeekSNI(server, 5*time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("PeekSNI blocked on non-TLS input — the plain-HTTP stall bug is back")
	}

	if !errors.Is(err, ErrNotTLSClientHello) {
		t.Fatalf("err = %v, want ErrNotTLSClientHello (sni=%q)", err, sni)
	}
	if sni != "" {
		t.Errorf("sni = %q, want empty for non-TLS", sni)
	}
	if string(peeked) != "GET /" {
		t.Errorf("peeked = %q, want the 5 header bytes %q so the caller can replay them", peeked, "GET /")
	}
}

// TestPeekSNI_NonTLSBinaryByte covers a non-0x16 first byte that is not HTTP
// either (e.g. a server-first protocol's stray byte) — same fast, no-body
// return path.
func TestPeekSNI_NonTLSBinaryByte(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() { _, _ = client.Write([]byte{0x00, 0x01, 0x02, 0x03, 0x04}) }()

	sni, peeked, err := PeekSNI(server, 2*time.Second)
	if !errors.Is(err, ErrNotTLSClientHello) {
		t.Fatalf("err = %v, want ErrNotTLSClientHello", err)
	}
	if sni != "" || len(peeked) != 5 {
		t.Errorf("sni=%q peeked=%v, want empty sni + 5 peeked bytes", sni, peeked)
	}
}
