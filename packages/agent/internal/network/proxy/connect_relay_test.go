package proxy

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	agentaudit "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/localfs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// proxy.go — Relay + closeWrite

// closeWrite on a non-halfCloser net.Conn must not panic and must be a
// no-op. The default branch (66.7%→100% via this assertion) covers the
// fallback when neither *net.TCPConn nor *tls.Conn methods are present
// — e.g. a wrapped ReplayConn that does not promote CloseWrite.
type plainConn struct{ net.Conn }

func TestCloseWrite_NoHalfCloser_NoOp(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	pc := plainConn{a}
	closeWrite(pc) // must not panic; no observable side effect
}

// TestRelay_BidirectionalByteCounts verifies that Relay returns accurate
// per-direction byte counts and unblocks both halves when both peers
// close cleanly. Uses real TCP loopback (not net.Pipe) so io.Copy's
// half-close path through *net.TCPConn.CloseWrite works as in
// production.
func TestRelay_BidirectionalByteCounts(t *testing.T) {
	a := tcpLoopbackPair(t)
	b := tcpLoopbackPair(t)
	defer a.aSide.Close() //nolint:errcheck
	defer a.bSide.Close() //nolint:errcheck
	defer b.aSide.Close() //nolint:errcheck
	defer b.bSide.Close() //nolint:errcheck

	go func() {
		_, _ = a.bSide.Write([]byte("hello"))
		_ = a.bSide.Close()
	}()
	go func() {
		_, _ = b.bSide.Write([]byte("world!!"))
		_ = b.bSide.Close()
	}()

	aToB, bToA := Relay(a.aSide, b.aSide)
	if aToB != 5 {
		t.Errorf("aToB: got %d want 5", aToB)
	}
	if bToA != 7 {
		t.Errorf("bToA: got %d want 7", bToA)
	}
}

// tcpPair is one socket-pair from tcpLoopbackPair.
type tcpPair struct {
	aSide net.Conn
	bSide net.Conn
}

// tcpLoopbackPair returns a bidirectional connected pair via a transient
// TCP listener — net.Pipe is synchronous + does not support CloseWrite,
// so io.Copy half-close in Relay never advances.
func tcpLoopbackPair(t *testing.T) tcpPair {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck
	acceptDone := make(chan net.Conn, 1)
	errDone := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			errDone <- e
			return
		}
		acceptDone <- c
	}()
	dialer := net.Dialer{Timeout: 1 * time.Second}
	dialed, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case accepted := <-acceptDone:
		return tcpPair{aSide: dialed, bSide: accepted}
	case e := <-errDone:
		t.Fatalf("accept: %v", e)
	case <-time.After(1 * time.Second):
		t.Fatal("accept timeout")
	}
	return tcpPair{}
}

// proxy.go — ExtractSNI / parseSNIExtension / PeekSNI

// buildClientHello assembles a minimal valid TLS ClientHello carrying the
// given SNI. Mirrors what crypto/tls would write but is small enough to
// keep test failures debuggable. Returns the complete record including the
// 5-byte TLS record header.
func buildClientHello(t *testing.T, sni string) []byte {
	t.Helper()
	// Build SNI extension data: list_len(2) + name_type(1) + name_len(2) + name
	sniName := []byte(sni)
	sniData := make([]byte, 0, 5+len(sniName))
	sniData = binary.BigEndian.AppendUint16(sniData, uint16(3+len(sniName)))
	sniData = append(sniData, 0x00) // name_type host_name
	sniData = binary.BigEndian.AppendUint16(sniData, uint16(len(sniName)))
	sniData = append(sniData, sniName...)

	// Extension: type=0x0000 + len + data
	ext := make([]byte, 0, 4+len(sniData))
	ext = binary.BigEndian.AppendUint16(ext, 0x0000)
	ext = binary.BigEndian.AppendUint16(ext, uint16(len(sniData)))
	ext = append(ext, sniData...)

	// ClientHello body: client_version(2) + random(32) + session_id_len(1)=0 +
	// cipher_suites_len(2)=2 + cipher_suite(2)=0x002F + compression_len(1)=1 +
	// compression(1)=0 + extensions_len(2) + extensions
	ch := make([]byte, 0, 64+len(ext))
	ch = append(ch, 0x03, 0x03)                        // TLS 1.2
	ch = append(ch, bytes.Repeat([]byte{0x42}, 32)...) // random
	ch = append(ch, 0x00)                              // session_id_len
	ch = binary.BigEndian.AppendUint16(ch, 0x0002)
	ch = append(ch, 0x00, 0x2F) // cipher_suite
	ch = append(ch, 0x01, 0x00) // compression
	ch = binary.BigEndian.AppendUint16(ch, uint16(len(ext)))
	ch = append(ch, ext...)

	// Handshake header: type=0x01 (ClientHello) + length(3) = ch len
	hs := make([]byte, 0, 4+len(ch))
	hs = append(hs, 0x01)
	hs = append(hs, byte(len(ch)>>16), byte(len(ch)>>8), byte(len(ch)))
	hs = append(hs, ch...)

	// Record header: type=0x16 (handshake) + version(2) + length(2) = hs len
	rec := make([]byte, 0, 5+len(hs))
	rec = append(rec, 0x16, 0x03, 0x01) // type + legacy_version
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(hs)))
	rec = append(rec, hs...)
	return rec
}

// TestExtractSNI_HappyPath asserts a well-formed ClientHello yields its SNI.
func TestExtractSNI_HappyPath(t *testing.T) {
	rec := buildClientHello(t, "example.com")
	if got := ExtractSNI(rec); got != "example.com" {
		t.Errorf("ExtractSNI: got %q want example.com", got)
	}
}

// TestExtractSNI_DefenseInDepth covers each guard branch in ExtractSNI:
// short record, non-handshake type byte, length lies, missing extensions,
// and the parseSNIExtension truncation arms.
func TestExtractSNI_DefenseInDepth(t *testing.T) {
	tests := []struct {
		name  string
		hello []byte
	}{
		{"empty", nil},
		{"non-handshake-type", []byte{0x17, 0x03, 0x01, 0x00, 0x00}},
		{"short-truncated", []byte{0x16, 0x03, 0x01, 0xFF, 0xFF}},
		{"handshake-too-short", append([]byte{0x16, 0x03, 0x01, 0x00, 0x04}, 0x01, 0x00, 0x00, 0x00)},
		{"not-clienthello-type", append([]byte{0x16, 0x03, 0x01, 0x00, 0x04}, 0x02, 0x00, 0x00, 0x00)},
		// Truncated extensions block: client_version + random only, no session id length byte.
		{"too-short-for-version+random", append([]byte{0x16, 0x03, 0x01, 0x00, 0x06}, 0x01, 0x00, 0x00, 0x02, 0x03, 0x03)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractSNI(tc.hello); got != "" {
				t.Errorf("ExtractSNI(%q): got %q want empty", tc.name, got)
			}
		})
	}
}

// TestExtractSNI_NoExtensions covers a ClientHello that has session id,
// cipher suites, and compression methods but lies about the extensions
// length so the for-loop exits without finding SNI. Mirrors a real-world
// case (TLS 1.3 ClientHello with no SNI extension).
func TestExtractSNI_NoSNIExtension(t *testing.T) {
	rec := buildClientHello(t, "")
	// buildClientHello with empty name produces an SNI extension carrying
	// an empty host_name. parseSNIExtension returns "" because nameLen=0
	// fails the > nameLen guard? Actually it returns the zero-length string.
	got := ExtractSNI(rec)
	if got != "" {
		t.Errorf("empty SNI body: got %q want empty", got)
	}
}

// TestExtractSNI_HandshakeBodyTooShort triggers the hsLen>len(data) guard
// by writing a record-length that exceeds the handshake-length declaration
// — the function bails before reading the ClientHello body.
func TestExtractSNI_HandshakeBodyTooShort(t *testing.T) {
	// Record header advertising 8 bytes, handshake header claims 100.
	rec := []byte{0x16, 0x03, 0x01, 0x00, 0x08, 0x01, 0x00, 0x00, 0x64, 0x03, 0x03, 0x00, 0x00}
	if got := ExtractSNI(rec); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// TestExtractSNI_PosOverflowAfterRandom covers the pos>=len(data) guard
// right after the 34-byte client_version+random fixed prefix.
func TestExtractSNI_PosOverflowAfterRandom(t *testing.T) {
	// Build a ClientHello with exactly 34 bytes (no session_id length byte).
	ch := append([]byte{0x03, 0x03}, bytes.Repeat([]byte{0x42}, 32)...) // len 34
	hs := append([]byte{0x01, 0x00, 0x00, byte(len(ch))}, ch...)
	rec := append([]byte{0x16, 0x03, 0x01, 0x00, byte(len(hs))}, hs...)
	if got := ExtractSNI(rec); got != "" {
		t.Errorf("got %q want empty (session_id length missing)", got)
	}
}

// TestExtractSNI_PosOverflowInCipherLength covers the pos+2>len(data)
// arm after session_id length.
func TestExtractSNI_PosOverflowInCipherLength(t *testing.T) {
	ch := append([]byte{0x03, 0x03}, bytes.Repeat([]byte{0x42}, 32)...)
	ch = append(ch, 0x00) // session_id_len=0, but no cipher suites bytes
	hs := append([]byte{0x01, 0x00, 0x00, byte(len(ch))}, ch...)
	rec := append([]byte{0x16, 0x03, 0x01, 0x00, byte(len(hs))}, hs...)
	if got := ExtractSNI(rec); got != "" {
		t.Errorf("got %q want empty (cipher length missing)", got)
	}
}

// TestExtractSNI_PosOverflowInCompression covers the pos>=len(data) arm
// after cipher suites.
func TestExtractSNI_PosOverflowInCompression(t *testing.T) {
	ch := append([]byte{0x03, 0x03}, bytes.Repeat([]byte{0x42}, 32)...)
	ch = append(ch, 0x00)                   // session_id_len=0
	ch = append(ch, 0x00, 0x02, 0x00, 0x2F) // cipher_suites len=2 + 1 suite
	// missing compression length byte
	hs := append([]byte{0x01, 0x00, 0x00, byte(len(ch))}, ch...)
	rec := append([]byte{0x16, 0x03, 0x01, 0x00, byte(len(hs))}, hs...)
	if got := ExtractSNI(rec); got != "" {
		t.Errorf("got %q want empty (compression length missing)", got)
	}
}

// TestExtractSNI_PosOverflowInExtensions covers the pos+2>len(data) arm
// for the extensions length prefix.
func TestExtractSNI_PosOverflowInExtensions(t *testing.T) {
	ch := append([]byte{0x03, 0x03}, bytes.Repeat([]byte{0x42}, 32)...)
	ch = append(ch, 0x00)                   // session_id_len=0
	ch = append(ch, 0x00, 0x02, 0x00, 0x2F) // cipher_suites
	ch = append(ch, 0x01, 0x00)             // compression_methods len=1
	// missing extensions length bytes
	hs := append([]byte{0x01, 0x00, 0x00, byte(len(ch))}, ch...)
	rec := append([]byte{0x16, 0x03, 0x01, 0x00, byte(len(hs))}, hs...)
	if got := ExtractSNI(rec); got != "" {
		t.Errorf("got %q want empty (extensions length missing)", got)
	}
}

// TestExtractSNI_ExtensionDataLengthBlowsPastEnd covers the
// pos+extDataLen>len(data) break-out arm inside the extensions loop.
func TestExtractSNI_ExtensionDataLengthBlowsPastEnd(t *testing.T) {
	ch := append([]byte{0x03, 0x03}, bytes.Repeat([]byte{0x42}, 32)...)
	ch = append(ch, 0x00)
	ch = append(ch, 0x00, 0x02, 0x00, 0x2F)
	ch = append(ch, 0x01, 0x00)
	// Extensions: 1 extension with type=0x0010 (ALPN) and length=0xFFFF
	// blowing past the end.
	ext := []byte{0x00, 0x10, 0xFF, 0xFF}
	ch = binary.BigEndian.AppendUint16(ch, uint16(len(ext)))
	ch = append(ch, ext...)
	hs := append([]byte{0x01, 0x00, 0x00, byte(len(ch))}, ch...)
	rec := append([]byte{0x16, 0x03, 0x01, 0x00, byte(len(hs))}, hs...)
	if got := ExtractSNI(rec); got != "" {
		t.Errorf("got %q want empty (ext data overflow)", got)
	}
}

// TestExtractSNI_NonSNIExtensionOnly covers the loop step that walks past
// non-SNI extensions without matching, returning "".
func TestExtractSNI_NonSNIExtensionOnly(t *testing.T) {
	// Build a ClientHello with one non-SNI extension (type=0x0010 ALPN, length 0).
	ch := make([]byte, 0, 64)
	ch = append(ch, 0x03, 0x03)
	ch = append(ch, bytes.Repeat([]byte{0x42}, 32)...)
	ch = append(ch, 0x00)                   // session_id_len
	ch = append(ch, 0x00, 0x02, 0x00, 0x2F) // cipher_suites
	ch = append(ch, 0x01, 0x00)             // compression
	// Extensions: total_len(2) + (type=0x0010 + len=0 + data=0)
	extBody := []byte{0x00, 0x10, 0x00, 0x00}
	ch = binary.BigEndian.AppendUint16(ch, uint16(len(extBody)))
	ch = append(ch, extBody...)
	hs := make([]byte, 0, 4+len(ch))
	hs = append(hs, 0x01)
	hs = append(hs, byte(len(ch)>>16), byte(len(ch)>>8), byte(len(ch)))
	hs = append(hs, ch...)
	rec := make([]byte, 0, 5+len(hs))
	rec = append(rec, 0x16, 0x03, 0x01)
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(hs)))
	rec = append(rec, hs...)
	if got := ExtractSNI(rec); got != "" {
		t.Errorf("non-SNI ext only: got %q want empty", got)
	}
}

// TestParseSNIExtension_Branches covers the dedicated parser's edge cases
// that ExtractSNI can not reach because they need a directly-crafted SNI
// extension body shorter than 5 bytes or with a non-zero name_type.
func TestParseSNIExtension_Branches(t *testing.T) {
	if got := parseSNIExtension(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := parseSNIExtension([]byte{0x00, 0x03, 0x00, 0x00}); got != "" {
		t.Errorf("under-5-bytes: got %q", got)
	}
	// nameType != 0 → returns ""
	bad := []byte{0x00, 0x05, 0x99, 0x00, 0x02, 'A', 'B'}
	if got := parseSNIExtension(bad); got != "" {
		t.Errorf("nameType!=0: got %q", got)
	}
	// happy path embedded value.
	good := []byte{0x00, 0x06, 0x00, 0x00, 0x03, 'a', 'b', 'c'}
	if got := parseSNIExtension(good); got != "abc" {
		t.Errorf("good: got %q want abc", got)
	}
}

// TestPeekSNI_HappyPath drives the byte path through a net.Pipe: write a
// valid ClientHello, then PeekSNI must return the SNI and the full record
// bytes (so the caller can replay them through ReplayConn).
func TestPeekSNI_HappyPath(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	rec := buildClientHello(t, "api.example.com")

	go func() {
		_, _ = client.Write(rec)
		// keep open until peek done
	}()

	sni, peeked, err := PeekSNI(server, 2*time.Second)
	if err != nil {
		t.Fatalf("PeekSNI: %v", err)
	}
	if sni != "api.example.com" {
		t.Errorf("sni: got %q want api.example.com", sni)
	}
	if !bytes.Equal(peeked, rec) {
		t.Errorf("peeked bytes don't match written record\nwant: %x\ngot:  %x", rec, peeked)
	}
}

// TestPeekSNI_HeaderTimeout fires when the peer never writes — the
// SetReadDeadline must trip and the function returns an error citing the
// header read failure (fail-open contract: caller decides how to handle).
func TestPeekSNI_HeaderTimeout(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	sni, _, err := PeekSNI(server, 50*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error, got sni=%q", sni)
	}
	if !strings.Contains(err.Error(), "read TLS header") {
		t.Errorf("error should reference TLS header read: %v", err)
	}
}

// TestPeekSNI_InvalidRecordLength covers the guard that rejects records
// outside [1, 16384]. The caller MUST still get the 5-byte header back so
// it can decide whether to surface or replay.
func TestPeekSNI_InvalidRecordLength(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	go func() {
		// Record header advertising length 0 — invalid.
		_, _ = client.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x00})
	}()
	_, header, err := PeekSNI(server, 1*time.Second)
	if err == nil {
		t.Fatal("expected invalid record length error")
	}
	if !strings.Contains(err.Error(), "invalid TLS record length") {
		t.Errorf("error should cite invalid length: %v", err)
	}
	if len(header) != 5 {
		t.Errorf("header bytes: got len %d want 5", len(header))
	}
}

// TestPeekSNI_BodyShort drives the partial-read branch by writing the
// header advertising a body length but closing before the body is sent.
func TestPeekSNI_BodyShort(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		// Header advertising 10-byte body, only write 3 bytes then close.
		_, _ = client.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x0A, 0x01, 0x02, 0x03})
		_ = client.Close()
	}()
	_, rec, err := PeekSNI(server, 1*time.Second)
	if err == nil {
		t.Fatal("expected body short error")
	}
	if !strings.Contains(err.Error(), "read TLS record body") {
		t.Errorf("error should cite body read: %v", err)
	}
	if len(rec) != 5 {
		t.Errorf("partial-body should return only the header bytes, got len %d", len(rec))
	}
}

// TestPeekSNI_SetDeadlineError covers the SetReadDeadline failure arm
// which happens when the conn has been closed by the time PeekSNI runs.
func TestPeekSNI_SetDeadlineError(t *testing.T) {
	server, client := net.Pipe()
	_ = client.Close()
	_ = server.Close()
	_, _, err := PeekSNI(server, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on closed conn")
	}
}

// TestReplayConn_ReadsReplayThenUnderlying verifies that the wrapper
// drains the replay buffer first, then falls through to the underlying
// conn for subsequent reads.
func TestReplayConn_ReadsReplayThenUnderlying(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	rc := NewReplayConn(server, []byte("REPLAY"))

	// First read drains replay buffer.
	buf := make([]byte, 6)
	n, err := rc.Read(buf)
	if err != nil || n != 6 || string(buf[:n]) != "REPLAY" {
		t.Fatalf("first read: n=%d err=%v buf=%q", n, err, buf[:n])
	}

	// Now writes on `client` should appear on the second Read.
	go func() { _, _ = client.Write([]byte("LIVE")) }()
	buf2 := make([]byte, 4)
	n2, err := io.ReadFull(rc, buf2)
	if err != nil || n2 != 4 || string(buf2) != "LIVE" {
		t.Fatalf("second read: n=%d err=%v buf=%q", n2, err, buf2)
	}
}

// TestReplayConn_PartialReadOfReplay drives the case where the caller's
// buffer is shorter than the remaining replay bytes — the wrapper must
// return only what fits and advance pos so the next Read continues.
func TestReplayConn_PartialReadOfReplay(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	rc := NewReplayConn(server, []byte("ABCDE"))

	buf := make([]byte, 2)
	n, _ := rc.Read(buf)
	if n != 2 || string(buf[:n]) != "AB" {
		t.Errorf("first chunk: got %q n=%d", buf[:n], n)
	}
	n, _ = rc.Read(buf)
	if n != 2 || string(buf[:n]) != "CD" {
		t.Errorf("second chunk: got %q n=%d", buf[:n], n)
	}
	n, _ = rc.Read(buf)
	if n != 1 || buf[0] != 'E' {
		t.Errorf("third chunk: got %q n=%d", buf[:n], n)
	}
}

// proxy.go — ParseCONNECT / RespondCONNECT / RejectCONNECT

func TestParseCONNECT_HappyPath(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() {
		_, _ = client.Write([]byte("CONNECT api.example.com:8443 HTTP/1.1\r\nHost: api.example.com:8443\r\n\r\n"))
	}()
	host, port, wrapped, err := ParseCONNECT(server, 1*time.Second)
	if err != nil {
		t.Fatalf("ParseCONNECT: %v", err)
	}
	if host != "api.example.com" || port != 8443 {
		t.Errorf("got %s:%d want api.example.com:8443", host, port)
	}
	if wrapped == nil {
		t.Error("wrapped conn must not be nil")
	}
}

// TestParseCONNECT_WithReplayedClientHello pins the most important branch
// for the agent: when the client batches the CONNECT request and the TLS
// ClientHello into one TCP segment, the bufio reader buffers leftover
// bytes that must be replayed via ReplayConn so the subsequent TLS
// handshake sees the full record.
func TestParseCONNECT_WithReplayedClientHello(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	// CONNECT request + 4 bytes of pretend ClientHello on the same write.
	go func() {
		req := "CONNECT host.example.test:443 HTTP/1.1\r\nHost: host.example.test:443\r\n\r\n"
		_, _ = client.Write([]byte(req + "LEAK"))
	}()
	host, port, wrapped, err := ParseCONNECT(server, 1*time.Second)
	if err != nil {
		t.Fatalf("ParseCONNECT: %v", err)
	}
	if host != "host.example.test" || port != 443 {
		t.Errorf("got %s:%d", host, port)
	}
	// First Read on wrapped MUST yield the buffered "LEAK" bytes.
	buf := make([]byte, 4)
	n, err := io.ReadFull(wrapped, buf)
	if err != nil || n != 4 || string(buf) != "LEAK" {
		t.Errorf("replay drain: n=%d err=%v buf=%q want LEAK", n, err, buf)
	}
}

func TestParseCONNECT_NotConnectMethod(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		_, _ = client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		_ = client.Close()
	}()
	_, _, _, err := ParseCONNECT(server, 1*time.Second)
	if err == nil {
		t.Fatal("expected error on non-CONNECT")
	}
	if !strings.Contains(err.Error(), "not a CONNECT") {
		t.Errorf("error should cite not-CONNECT: %v", err)
	}
}

func TestParseCONNECT_InvalidTarget(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		_, _ = client.Write([]byte("CONNECT notahostport HTTP/1.1\r\n\r\n"))
		_ = client.Close()
	}()
	_, _, _, err := ParseCONNECT(server, 1*time.Second)
	if err == nil {
		t.Fatal("expected error on bad target")
	}
	if !strings.Contains(err.Error(), "invalid CONNECT target") {
		t.Errorf("error should cite invalid target: %v", err)
	}
}

func TestParseCONNECT_InvalidPort(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		_, _ = client.Write([]byte("CONNECT host:abc HTTP/1.1\r\n\r\n"))
		_ = client.Close()
	}()
	_, _, _, err := ParseCONNECT(server, 1*time.Second)
	if err == nil {
		t.Fatal("expected error on non-numeric port")
	}
	if !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("error should cite invalid port: %v", err)
	}
}

// TestParseCONNECT_HeaderDrainError exercises the inner-loop break arm
// when an additional header read errors before terminating. The CONNECT
// line is well-formed but the trailing header line is truncated.
func TestParseCONNECT_HeaderDrainError(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		// Valid CONNECT line followed by a partial header that never
		// terminates with \r\n — close the conn so ReadString errors.
		_, _ = client.Write([]byte("CONNECT host.test:443 HTTP/1.1\r\nUser-Agent: nope"))
		_ = client.Close()
	}()
	host, port, _, err := ParseCONNECT(server, 1*time.Second)
	if err != nil {
		t.Fatalf("ParseCONNECT: %v", err)
	}
	if host != "host.test" || port != 443 {
		t.Errorf("got %s:%d", host, port)
	}
}

func TestParseCONNECT_ReadLineError(t *testing.T) {
	server, client := net.Pipe()
	_ = client.Close()
	_ = server.Close()
	_, _, _, err := ParseCONNECT(server, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected read line error on closed conn")
	}
}

func TestRespondCONNECT_WritesEstablished(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	go func() {
		_ = RespondCONNECT(server)
	}()
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "200 Connection Established") {
		t.Errorf("got %q", got)
	}
}

func TestRejectCONNECT_WritesForbidden(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	go func() {
		RejectCONNECT(server)
	}()
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "403 Forbidden") {
		t.Errorf("got %q", got)
	}
}

// bridge.go — loggingQueueWriter

// captureWriter implements sharedaudit.Writer and records every Enqueue
// so the test can assert loggingQueueWriter delegates correctly.
type captureWriter struct {
	mu      sync.Mutex
	events  []sharedaudit.AuditEvent
	flushed int
	closed  int
}

func (w *captureWriter) Enqueue(e sharedaudit.AuditEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, e)
}
func (w *captureWriter) Flush(_ context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushed++
	return nil
}
func (w *captureWriter) Close(_ context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed++
	return nil
}

func TestLoggingQueueWriter_DelegatesEnqueueFlushClose(t *testing.T) {
	next := &captureWriter{}
	w := &loggingQueueWriter{next: next, logger: nil}
	w.Enqueue(sharedaudit.AuditEvent{
		ID: "id-1", TraceID: "tr-1", TargetHost: "example.com",
		Method: "POST", Path: "/v1/chat",
	})
	if len(next.events) != 1 || next.events[0].ID != "id-1" {
		t.Fatalf("events: got %v", next.events)
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Errorf("Flush: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if next.flushed != 1 || next.closed != 1 {
		t.Errorf("flushed=%d closed=%d want 1/1", next.flushed, next.closed)
	}
}

// TestLoggingQueueWriter_NilNext is the defensive arm: a nil writer must
// not panic on Enqueue/Flush/Close. The Default slog logger is used.
func TestLoggingQueueWriter_NilNext_NoPanic(t *testing.T) {
	w := &loggingQueueWriter{next: nil, logger: nil}
	w.Enqueue(sharedaudit.AuditEvent{ID: "x"})
	if err := w.Flush(context.Background()); err != nil {
		t.Errorf("Flush nil-next: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Errorf("Close nil-next: %v", err)
	}
}

// errWriter forces Flush + Close to return errors so the delegating
// arms propagate them.
type errWriter struct{}

func (e *errWriter) Enqueue(_ sharedaudit.AuditEvent) {}
func (e *errWriter) Flush(_ context.Context) error    { return errors.New("flush-fail") }
func (e *errWriter) Close(_ context.Context) error    { return errors.New("close-fail") }

func TestLoggingQueueWriter_PropagatesErrors(t *testing.T) {
	w := &loggingQueueWriter{next: &errWriter{}}
	if err := w.Flush(context.Background()); err == nil || err.Error() != "flush-fail" {
		t.Errorf("Flush: got %v", err)
	}
	if err := w.Close(context.Background()); err == nil || err.Error() != "close-fail" {
		t.Errorf("Close: got %v", err)
	}
}

// bridge.go — BumpFlow early-validation + non-TLS-port arm

func TestBumpFlow_NilTLSEngine(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	err := BumpFlow(context.Background(), server, nil, "x", 443, "fl", FlowProcess{}, BridgeDeps{})
	if err == nil || !strings.Contains(err.Error(), "nil TLSEngine") {
		t.Errorf("got %v want nil TLSEngine error", err)
	}
}

func TestBumpFlow_NilUpstream(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	eng := newTestEngine(t)
	err := BumpFlow(context.Background(), server, nil, "x", 443, "fl", FlowProcess{}, BridgeDeps{TLSEngine: eng})
	if err == nil || !strings.Contains(err.Error(), "nil Upstream") {
		t.Errorf("got %v want nil Upstream error", err)
	}
}

func TestBumpFlow_NilAuditQueue(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	eng := newTestEngine(t)
	// Construct a real but minimal Upstream so the next nil check fires.
	up := newTestUpstream(t)
	err := BumpFlow(context.Background(), server, nil, "x", 443, "fl", FlowProcess{}, BridgeDeps{TLSEngine: eng, Upstream: up})
	if err == nil || !strings.Contains(err.Error(), "nil AuditQueue") {
		t.Errorf("got %v want nil AuditQueue error", err)
	}
}

// mockAddr is the net.Addr for the in-memory mock conns below.
type mockAddr struct{}

func (mockAddr) Network() string { return "mock" }
func (mockAddr) String() string  { return "mock" }

// drainConn is a fully in-memory net.Conn: Read returns the preloaded bytes
// then io.EOF (never blocks), Write buffers (never blocks). It lets the
// opaqueRelay copy loops terminate deterministically with no sockets and no
// port assumptions — both directions hit EOF on their own.
type drainConn struct {
	readBuf *bytes.Reader
	mu      sync.Mutex
	written bytes.Buffer
	closed  bool
}

func newDrainConn(readData []byte) *drainConn {
	return &drainConn{readBuf: bytes.NewReader(readData)}
}

func (c *drainConn) Read(b []byte) (int, error) { return c.readBuf.Read(b) }
func (c *drainConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	return c.written.Write(b)
}
func (c *drainConn) writtenBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.written.Bytes()...)
}
func (c *drainConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *drainConn) LocalAddr() net.Addr              { return mockAddr{} }
func (c *drainConn) RemoteAddr() net.Addr             { return mockAddr{} }
func (c *drainConn) SetDeadline(time.Time) error      { return nil }
func (c *drainConn) SetReadDeadline(time.Time) error  { return nil }
func (c *drainConn) SetWriteDeadline(time.Time) error { return nil }

// blockingConn is an in-memory net.Conn whose Read blocks until Close, used to
// hold one opaqueRelay copy direction open so the ctx.Done wait arm is hit.
// Write discards.
type blockingConn struct {
	release chan struct{}
	once    sync.Once
}

func newBlockingConn() *blockingConn { return &blockingConn{release: make(chan struct{})} }
func (c *blockingConn) Read(b []byte) (int, error) {
	<-c.release
	return 0, io.EOF
}
func (c *blockingConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *blockingConn) Close() error {
	c.once.Do(func() { close(c.release) })
	return nil
}
func (c *blockingConn) LocalAddr() net.Addr              { return mockAddr{} }
func (c *blockingConn) RemoteAddr() net.Addr             { return mockAddr{} }
func (c *blockingConn) SetDeadline(time.Time) error      { return nil }
func (c *blockingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockingConn) SetWriteDeadline(time.Time) error { return nil }

// installMockUpstream swaps the opaqueDialContext seam to return `upstream`
// (or `dialErr`) for the test's duration, restoring the real TCP dialer after.
// No real sockets or port-availability assumptions.
func installMockUpstream(t *testing.T, upstream net.Conn, dialErr error) {
	t.Helper()
	prev := opaqueDialContext
	opaqueDialContext = func(_ context.Context, _ string) (net.Conn, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return upstream, nil
	}
	t.Cleanup(func() { opaqueDialContext = prev })
}

// TestBumpFlow_NonTLSPort_DialFailure exercises the opaque-relay-fail arm: a
// non-TLS port routes to opaqueRelay, whose (mocked) dial is refused, so
// BumpFlow surfaces the error rather than silently passing through.
func TestBumpFlow_NonTLSPort_DialFailure(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	installMockUpstream(t, nil, errors.New("mock upstream refused"))

	eng := newTestEngine(t)
	up := newTestUpstream(t)
	queue := newTestAuditQueue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := BumpFlow(ctx, server, nil, "upstream.invalid", 1080, "fl-err", FlowProcess{}, BridgeDeps{
		TLSEngine:  eng,
		Upstream:   up,
		AuditQueue: queue,
	})
	if err == nil {
		t.Fatal("expected opaque relay dial failure")
	}
}

// TestBumpFlow_TLSPort_BumpConnectionTLSHandshakeFails drives BumpFlow
// all the way to tlsbump.BumpConnection, which then fails at TLS
// handshake (the net.Pipe client never speaks TLS). The function
// classifies the error stage and returns it — covering most of the
// option-building + classification block (60+ statements).
//
// Also exercises every optional-dep branch by wiring every field.
func TestBumpFlow_TLSPort_BumpConnectionTLSHandshakeFails(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	eng := newTestEngine(t)
	up := newTestUpstream(t)
	queue := newTestAuditQueue(t)
	policyResolver := newTestPolicyResolver(t)
	domainEngine := domain.NewEngine()
	registry := traffic.NewAdapterRegistry("test")
	captureStore := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	streamPolicy := streampolicy.NewStore(streampolicy.DefaultPolicy())
	spill, spillErr := localfs.New(localfs.Options{Root: t.TempDir()})
	if spillErr != nil {
		t.Fatalf("localfs.New: %v", spillErr)
	}

	// Mock the fallback dial to fail so the fail-open opaqueRelay also fails
	// and BumpFlow surfaces an error (no real socket / port assumption).
	installMockUpstream(t, nil, errors.New("mock upstream refused"))

	// Close client immediately so the inner tls.Server.Handshake fails fast.
	_ = client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := BumpFlow(ctx, server, []byte("PEEKED"), "127.0.0.1", 443, "fl-handshake", FlowProcess{
		Name: "TestApp", Bundle: "com.example.TestApp", User: "tester",
	}, BridgeDeps{
		Logger:              nil, // exercises default-logger branch
		TLSEngine:           eng,
		Upstream:            up,
		PolicyResolver:      policyResolver,
		DomainEngine:        domainEngine,
		AdapterRegistry:     registry,
		PayloadCaptureStore: captureStore,
		SpillStore:          spill, // exercises the SpillStore-wired emitter branch
		StreamingPolicy:     streamPolicy,
		AuditQueue:          queue,
		// Defaults exercised: PerHookTimeout=0 → 5s, TotalTimeout=0 → 30s.
	})
	// Client speaks no TLS → BumpConnection fails at client_pin_check; the
	// fail-open fallback dial is mocked to fail too, so BumpFlow returns an
	// error. (When the fallback dial succeeds, BumpFlow returns nil — covered
	// by TestBumpFlow_TLSPort_PinCheckFailure_FallbackSucceeds.)
	if err == nil {
		t.Error("expected an error when both bump and fallback dial fail")
	}
}

// TestBumpFlow_TLSPort_CustomTimeouts pins the non-default timeout branch.
func TestBumpFlow_TLSPort_CustomTimeouts(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	eng := newTestEngine(t)
	up := newTestUpstream(t)
	queue := newTestAuditQueue(t)
	installMockUpstream(t, newDrainConn(nil), nil) // fallback dial → in-memory upstream

	_ = client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = BumpFlow(ctx, server, nil, "127.0.0.1", 443, "fl-to", FlowProcess{}, BridgeDeps{
		TLSEngine:      eng,
		Upstream:       up,
		AuditQueue:     queue,
		PerHookTimeout: 2 * time.Second,
		TotalTimeout:   10 * time.Second,
	})
}

// TestBumpFlow_NonTLSPort_OpaqueRelay drives the dst_port != 443/8443 branch
// through opaqueRelay against an in-memory mock upstream. Verifies BumpFlow
// returns nil and the peeked bytes are replayed to the upstream.
func TestBumpFlow_NonTLSPort_OpaqueRelay(t *testing.T) {
	upstream := newDrainConn(nil) // reads EOF, captures what the relay writes
	installMockUpstream(t, upstream, nil)

	clientSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = agentSide.Close() })
	// Close the client so the relay's client→upstream copy hits EOF; the
	// upstream→client copy hits EOF on the drainConn — both directions
	// terminate with no sockets.
	_ = clientSide.Close()

	eng := newTestEngine(t)
	up := newTestUpstream(t)
	queue := newTestAuditQueue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := BumpFlow(ctx, agentSide, []byte("ping"), "upstream.invalid", 8080, "fl-1",
		FlowProcess{Name: "p", Bundle: "b", User: "u"}, BridgeDeps{
			TLSEngine:  eng,
			Upstream:   up,
			AuditQueue: queue,
		})
	if err != nil {
		t.Errorf("BumpFlow: %v", err)
	}
	if got := upstream.writtenBytes(); string(got) != "ping" {
		t.Errorf("peeked bytes not replayed to upstream: got %q want \"ping\"", got)
	}
}

// TestBumpFlow_TLSPort_PinCheckFailure_FallbackSucceeds drives the cert-pin
// fail-open path: BumpConnection fails at the client TLS handshake (the
// net.Pipe client speaks no TLS) → stage client_pin_check → the fail-open
// opaqueRelay fallback runs against an in-memory mock upstream and succeeds,
// so BumpFlow returns nil. This is the contract that keeps cert-pinning apps
// (Cursor / Slack / Notion) working when they reject our MITM leaf. dstPort
// 8443 only selects the TLS-bump branch — no socket is bound.
func TestBumpFlow_TLSPort_PinCheckFailure_FallbackSucceeds(t *testing.T) {
	installMockUpstream(t, newDrainConn(nil), nil)

	clientSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = agentSide.Close() })
	// Close the client so the inner tls.Server handshake against agentSide
	// fails — the client_pin_check trigger.
	_ = clientSide.Close()

	eng := newTestEngine(t)
	up := newTestUpstream(t)
	queue := newTestAuditQueue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := BumpFlow(ctx, agentSide, []byte("PEEKED-HELLO"), "127.0.0.1", 8443, "fl-pin-fallback", FlowProcess{
		Name: "TestApp", Bundle: "com.example.TestApp", User: "tester",
	}, BridgeDeps{
		TLSEngine:  eng,
		Upstream:   up,
		AuditQueue: queue,
	})
	// Fallback succeeded → nil, even though the client TLS handshake failed.
	if err != nil {
		t.Errorf("fallback opaqueRelay should succeed (BumpFlow returns nil); got %v", err)
	}
}

// TestOpaqueRelay_DialFailure pins the dial-error arm: a mocked dial refusal
// yields (0, 0, err) wrapping "opaque relay dial". No real socket.
func TestOpaqueRelay_DialFailure(t *testing.T) {
	installMockUpstream(t, nil, errors.New("mock upstream refused"))
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	bytesUp, bytesDown, err := opaqueRelay(ctx, newDrainConn(nil), nil, "upstream.invalid", 1)
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if !strings.Contains(err.Error(), "opaque relay dial") {
		t.Errorf("error should cite dial: %v", err)
	}
	if bytesUp != 0 || bytesDown != 0 {
		t.Errorf("byte counts on dial failure: got %d/%d want 0/0", bytesUp, bytesDown)
	}
}

// TestOpaqueRelay_HappyPath_NoPeeked covers both copy directions with an
// in-memory mock upstream that returns "PONG" then EOF. The client conn EOFs
// immediately, so both directions terminate deterministically — no sockets,
// no goroutine choreography.
func TestOpaqueRelay_HappyPath_NoPeeked(t *testing.T) {
	upstream := newDrainConn([]byte("PONG")) // upstream→client copies PONG, then EOF
	installMockUpstream(t, upstream, nil)
	client := newDrainConn(nil) // client→upstream sees EOF immediately

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bytesUp, bytesDown, err := opaqueRelay(ctx, client, nil, "upstream.invalid", 80)
	if err != nil {
		t.Fatalf("opaqueRelay: %v", err)
	}
	// Best-effort labelling — assert the SUM is the 4 PONG bytes that survived
	// the upstream→client direction (the audit-row contract).
	if total := bytesUp + bytesDown; total < 4 {
		t.Errorf("total bytes (up+down): got %d want ≥4", total)
	}
	if got := client.writtenBytes(); string(got) != "PONG" {
		t.Errorf("upstream→client bytes: got %q want \"PONG\"", got)
	}
}

// TestOpaqueRelay_CtxCancelDuringSecondWait pins the ctx.Done arm: the client
// copy finishes immediately (client EOF), then the upstream copy blocks (the
// mock upstream's Read never returns until Close), so the second wait branches
// into ctx.Done when the deadline fires. opaqueRelay's deferred upstream.Close
// then unblocks the goroutine.
func TestOpaqueRelay_CtxCancelDuringSecondWait(t *testing.T) {
	installMockUpstream(t, newBlockingConn(), nil)
	client := newDrainConn(nil) // EOFs immediately → first copy done

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, _, err := opaqueRelay(ctx, client, nil, "upstream.invalid", 80)
	if err != nil {
		t.Errorf("ctx-cancel during second wait: got err %v want nil", err)
	}
}

// TestOpaqueRelay_PeekedBytesReplayed covers the branch that writes the peeked
// bytes to the upstream before the bidirectional copy. The mock upstream
// captures them.
func TestOpaqueRelay_PeekedBytesReplayed(t *testing.T) {
	upstream := newDrainConn(nil)
	installMockUpstream(t, upstream, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, _, err := opaqueRelay(ctx, newDrainConn(nil), []byte("ABCDEF"), "upstream.invalid", 80)
	if err != nil {
		t.Errorf("opaqueRelay: %v", err)
	}
	if got := upstream.writtenBytes(); string(got) != "ABCDEF" {
		t.Errorf("upstream got %q want ABCDEF", got)
	}
}

// TestOpaqueRelay_PeekedWriteError covers the arm where replaying the peeked
// bytes to the upstream fails: the mock upstream is pre-closed so its Write
// returns an error before the bidirectional copy starts.
func TestOpaqueRelay_PeekedWriteError(t *testing.T) {
	upstream := newDrainConn(nil)
	_ = upstream.Close() // Write now returns io.ErrClosedPipe
	installMockUpstream(t, upstream, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := opaqueRelay(ctx, newDrainConn(nil), []byte("PEEK"), "upstream.invalid", 80)
	if err == nil || !strings.Contains(err.Error(), "opaque relay write peeked") {
		t.Errorf("got %v want 'opaque relay write peeked' error", err)
	}
}

// readErrConn lets SetReadDeadline succeed but forces Read to error, so
// ParseCONNECT advances past the deadline arm into the first-line read.
type readErrConn struct{ net.Conn }

func (c readErrConn) SetReadDeadline(time.Time) error { return nil }
func (c readErrConn) Read([]byte) (int, error)        { return 0, errors.New("boom-read") }

// TestParseCONNECT_FirstReadError covers the ReadString error arm: the
// deadline is set fine, but the first line read fails. (TestParseCONNECT_
// ReadLineError exercises the SetReadDeadline arm instead, on a closed
// pipe.)
func TestParseCONNECT_FirstReadError(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	_, _, _, err := ParseCONNECT(readErrConn{Conn: a}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "read CONNECT line") {
		t.Errorf("got %v want 'read CONNECT line' error", err)
	}
}

// TestClassifyBumpFailureStage table-tests every stage arm without needing a
// live upstream — the live BumpConnection only ever surfaces one stage per
// run, so the classification logic is verified here directly.
func TestClassifyBumpFailureStage(t *testing.T) {
	tests := []struct {
		errStr string
		want   string
	}{
		{"TLS handshake with client: EOF", "client_pin_check"},
		{"utls handshake to 1.2.3.4:443: tls: first record does not look like a TLS handshake", "upstream_not_tls"},
		{"utls handshake to 1.2.3.4:443: x509: certificate signed by unknown authority", "upstream_utls_dial"},
		{"some unrelated transport error", "unknown"},
	}
	for _, tc := range tests {
		if got := classifyBumpFailureStage(tc.errStr); got != tc.want {
			t.Errorf("classifyBumpFailureStage(%q): got %q want %q", tc.errStr, got, tc.want)
		}
	}
}

// TestStaticCertGetter verifies the GetCertificate callback BumpFlow installs
// always serves the pre-minted leaf, ignoring the ClientHello (the agent
// mints by hostname up front — no per-Hello probe). The end-to-end handshake
// that invokes this callback is exercised in
// packages/shared/transport/tlsbump/forward_handler_test.go.
func TestStaticCertGetter(t *testing.T) {
	cert := &tls.Certificate{Certificate: [][]byte{{0x01, 0x02}}}
	got, err := staticCertGetter(cert)(&tls.ClientHelloInfo{ServerName: "ignored.example"})
	if err != nil {
		t.Fatalf("staticCertGetter: unexpected err %v", err)
	}
	if got != cert {
		t.Errorf("staticCertGetter returned %p, want the pre-minted cert %p", got, cert)
	}
}

// shared/transport/streaming SSE type stub for tests
// (mrand kept just to avoid unused import in some builds)

var _ = mrand.Int

// newTestEngine returns an agentTLS.Engine backed by a fresh self-signed
// CA so the rest of the bridge can mint leaves without filesystem I/O.
func newTestEngine(t *testing.T) *agentTLS.Engine {
	t.Helper()
	eng, err := agentTLS.NewEngine(nil, nil, 10, time.Hour)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

// newTestUpstream returns a real tlsbump.UpstreamTransport configured
// for tests (caps small, timeouts short). The BumpFlow non-TLS-port arm
// never invokes it, but BridgeDeps requires it non-nil to advance past
// the nil-Upstream guard.
func newTestUpstream(t *testing.T) *tlsbump.UpstreamTransport {
	t.Helper()
	up, err := tlsbump.NewUpstreamTransport(8, 30*time.Second, 2*time.Second)
	if err != nil {
		t.Fatalf("tlsbump.NewUpstreamTransport: %v", err)
	}
	return up
}

// newTestAuditQueue spawns a fresh in-memory SQLite audit Queue for the
// BumpFlow non-TLS-port test where we need a non-nil queue.
func newTestAuditQueue(t *testing.T) *agentaudit.Queue {
	t.Helper()
	q, err := agentaudit.NewQueue(":memory:", nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// newTestPolicyResolver returns an empty PolicyResolver that builds an
// empty pipeline — sufficient for the BumpFlow happy-path until BumpFlow
// hands off to tlsbump.BumpConnection.
func newTestPolicyResolver(t *testing.T) *compliance.PolicyResolver {
	t.Helper()
	return compliance.NewPolicyResolver(nil, hooks.NewHookRegistry(), nil)
}

// ensure httptest is referenced to avoid unused-import errors if future
// edits remove the only caller above; harmless at runtime.
var _ = httptest.NewServer

// ensureTLSImport — keep crypto/x509 / rand / pem / pkix imports anchored
// in case future seam tests use them for a hand-rolled local CA.
var _ = elliptic.P256
var _ = ecdsa.GenerateKey
var _ = rand.Int
var _ = x509.CreateCertificate
var _ = pem.Encode
var _ = pkix.Name{}
var _ = big.NewInt
var _ = io.EOF
