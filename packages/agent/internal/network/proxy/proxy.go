// Package proxy provides shared transparent-proxy utilities used by the
// platform interception shims: bidirectional TCP relay, TLS SNI peek /
// extraction, CONNECT parsing, and the replay-conn helper that lets a peeked
// ClientHello flow back through the real handshake. TLS-bump inspection lives
// in BumpFlow (bridge.go) → shared/tlsbump.
package proxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// tlsRecordTypeHandshake is the TLS record content type for a handshake
// record — the only record that can carry a ClientHello / SNI.
const tlsRecordTypeHandshake = 0x16

// ErrNotTLSClientHello is returned by PeekSNI when the first bytes on the
// connection are not a TLS handshake record (plaintext HTTP, or a
// server-speaks-first protocol where the client has not sent a ClientHello).
// Callers treat it like any other peek failure: skip inspection and pass the
// flow through, replaying the peeked bytes.
var ErrNotTLSClientHello = errors.New("proxy: not a TLS ClientHello")

// Relay copies data bidirectionally between two connections.
// Blocks until both directions complete. Returns bytes transferred.
func Relay(a, b net.Conn) (aToB, bToA int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		aToB, _ = io.Copy(b, a)
		closeWrite(b)
	}()

	go func() {
		defer wg.Done()
		bToA, _ = io.Copy(a, b)
		closeWrite(a)
	}()

	wg.Wait()
	return
}

// closeWrite signals half-close on the write side for both plain TCP and TLS
// connections to prevent goroutine leaks when one relay direction finishes.
func closeWrite(conn net.Conn) {
	type halfCloser interface {
		CloseWrite() error
	}
	if hc, ok := conn.(halfCloser); ok {
		_ = hc.CloseWrite()
	}
}

// PeekSNI reads the TLS ClientHello from a connection to extract the SNI
// hostname. Returns the peeked bytes which must be replayed to the actual
// TLS handshake via ReplayConn.
//
// Reads the 5-byte TLS record header first, then exactly recordLen more bytes
// via io.ReadFull to handle partial reads on slow connections correctly.
func PeekSNI(conn net.Conn, timeout time.Duration) (sni string, peeked []byte, err error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", nil, fmt.Errorf("set read deadline: %w", err)
	}
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	// TLS record: type(1) + version(2) + length(2)
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", nil, fmt.Errorf("read TLS header: %w", err)
	}

	// Only a TLS handshake record (content type 0x16, major version 0x03) can
	// carry a ClientHello. For anything else — plaintext HTTP, or a
	// server-speaks-first protocol where the client sent no ClientHello —
	// return the 5 peeked bytes and stop. Continuing would read header[3:5] as
	// a record length and block on bytes that never come (server-first) or
	// consume and corrupt the request body (e.g. "GET /" parses to an 8 KB
	// "length"), which manifests as a ~5 s stall and a broken plain-HTTP flow.
	if header[0] != tlsRecordTypeHandshake || header[1] != 0x03 {
		return "", header, ErrNotTLSClientHello
	}

	recordLen := int(header[3])<<8 | int(header[4])
	if recordLen < 1 || recordLen > 16384 {
		return "", header, fmt.Errorf("invalid TLS record length: %d", recordLen)
	}

	record := make([]byte, 5+recordLen)
	copy(record, header)
	if _, err := io.ReadFull(conn, record[5:]); err != nil {
		return "", record[:5], fmt.Errorf("read TLS record body: %w", err)
	}

	sni = ExtractSNI(record)
	return sni, record, nil
}

// ExtractSNI parses the SNI extension from a TLS ClientHello record.
// Returns "" if the data is not a valid ClientHello or has no SNI.
func ExtractSNI(hello []byte) string {
	// TLS record header: type(1) + version(2) + length(2)
	if len(hello) < 5 || hello[0] != 0x16 {
		return ""
	}
	recordLen := int(binary.BigEndian.Uint16(hello[3:5]))
	if len(hello) < 5+recordLen {
		return ""
	}
	data := hello[5 : 5+recordLen]

	// Handshake header: type(1) + length(3)
	if len(data) < 4 || data[0] != 0x01 {
		return ""
	}
	hsLen := int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if len(data) < 4+hsLen {
		return ""
	}
	data = data[4 : 4+hsLen]

	// ClientHello: version(2) + random(32) = 34 bytes minimum
	if len(data) < 34 {
		return ""
	}
	pos := 34

	// Session ID (length-prefixed)
	if pos >= len(data) {
		return ""
	}
	pos += 1 + int(data[pos])

	// Cipher suites (2-byte length prefix)
	if pos+2 > len(data) {
		return ""
	}
	pos += 2 + int(binary.BigEndian.Uint16(data[pos:]))

	// Compression methods (1-byte length prefix)
	if pos >= len(data) {
		return ""
	}
	pos += 1 + int(data[pos])

	// Extensions (2-byte length prefix)
	if pos+2 > len(data) {
		return ""
	}
	extLen := int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	extEnd := pos + extLen

	for pos+4 <= extEnd && pos+4 <= len(data) {
		extType := binary.BigEndian.Uint16(data[pos:])
		extDataLen := int(binary.BigEndian.Uint16(data[pos+2:]))
		pos += 4
		if pos+extDataLen > len(data) {
			break
		}
		if extType == 0x0000 { // SNI extension
			return parseSNIExtension(data[pos : pos+extDataLen])
		}
		pos += extDataLen
	}
	return ""
}

func parseSNIExtension(ext []byte) string {
	// SNI list: total_length(2) then entries: type(1) + name_length(2) + name
	if len(ext) < 5 {
		return ""
	}
	// len(ext) >= 5 (guard above) guarantees ext[2], ext[3], ext[4] exist —
	// enough for name_type(1) + name_length(2).
	entryPos := 2 // skip list length
	nameType := ext[entryPos]
	nameLen := int(binary.BigEndian.Uint16(ext[entryPos+1:]))
	entryPos += 3
	if nameType == 0 && entryPos+nameLen <= len(ext) {
		return string(ext[entryPos : entryPos+nameLen])
	}
	return ""
}

// ReplayConn wraps a net.Conn and prepends buffered data to reads.
// Used to replay peeked ClientHello bytes back through the TLS handshake.
type ReplayConn struct {
	net.Conn
	replay []byte
	pos    int
}

// NewReplayConn creates a connection that replays data before reading from conn.
func NewReplayConn(conn net.Conn, replay []byte) *ReplayConn {
	return &ReplayConn{Conn: conn, replay: replay}
}

func (c *ReplayConn) Read(b []byte) (int, error) {
	if c.pos < len(c.replay) {
		n := copy(b, c.replay[c.pos:])
		c.pos += n
		return n, nil
	}
	return c.Conn.Read(b)
}

// ParseCONNECT reads an HTTP CONNECT request from the connection using buffered
// I/O to handle partial TCP reads correctly. Returns the target host, port, and
// a wrapped connection that replays any buffered-but-unconsumed bytes (e.g. TLS
// ClientHello sent in the same TCP segment as the CONNECT request).
func ParseCONNECT(conn net.Conn, timeout time.Duration) (host string, port int, wrappedConn net.Conn, err error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", 0, nil, err
	}
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	reader := bufio.NewReaderSize(conn, 4096)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", 0, nil, fmt.Errorf("read CONNECT line: %w", err)
	}

	// Parse "CONNECT host:port HTTP/1.x\r\n"
	var method, target, proto string
	_, scanErr := fmt.Sscanf(line, "%s %s %s", &method, &target, &proto)
	if scanErr != nil || method != "CONNECT" {
		return "", 0, nil, fmt.Errorf("not a CONNECT request: %.40s", line)
	}

	// Drain remaining headers (terminated by empty line)
	for {
		hdr, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(hdr) == "" {
			break
		}
	}

	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, nil, fmt.Errorf("invalid CONNECT target %q: %w", target, err)
	}
	portNum := 443
	if _, err := fmt.Sscanf(p, "%d", &portNum); err != nil {
		return "", 0, nil, fmt.Errorf("invalid port %q: %w", p, err)
	}

	// Wrap the connection: if the bufio.Reader buffered extra bytes (e.g. TLS
	// ClientHello), they must be replayed before reading from the raw conn.
	if reader.Buffered() > 0 {
		buffered := make([]byte, reader.Buffered())
		if _, err := io.ReadFull(reader, buffered); err != nil {
			return "", 0, nil, fmt.Errorf("drain buffered bytes: %w", err)
		}
		wrappedConn = NewReplayConn(conn, buffered)
	} else {
		wrappedConn = conn
	}
	return h, portNum, wrappedConn, nil
}

// RespondCONNECT sends the HTTP 200 Connection Established response.
func RespondCONNECT(conn net.Conn) error {
	_, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	return err
}

// RejectCONNECT sends an HTTP 403 Forbidden response and closes the connection.
func RejectCONNECT(conn net.Conn) {
	_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")) // best-effort: client may already have disconnected on the reject path
}
