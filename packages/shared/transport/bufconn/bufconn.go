// Package bufconn wraps a net.Conn and prepends pre-buffered bytes to the
// read stream. Used after http.Hijacker.Hijack() (compliance-proxy) and
// after a Swift NE bridge ClientHello peek (agent) to preserve bytes that
// were read off the TCP stream before TLS handshake takes over.
//
// Without this wrapper, those pre-read bytes are silently lost and the
// downstream tls.Server handshake receives a truncated ClientHello.
package bufconn

import (
	"io"
	"net"
)

// Conn wraps a net.Conn so reads drain a buffered prefix before falling
// through to the underlying connection's Read.
type Conn struct {
	net.Conn
	reader io.Reader
}

// New wraps conn. If prefix is empty, conn is returned unchanged.
func New(conn net.Conn, prefix []byte) net.Conn {
	if len(prefix) == 0 {
		return conn
	}
	return &Conn{
		Conn: conn,
		reader: io.MultiReader(io.LimitReader(
			&bytesReader{data: prefix}, int64(len(prefix)),
		), conn),
	}
}

// Read reads from the buffered prefix until exhausted, then from the
// wrapped connection.
func (c *Conn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

// bytesReader is a minimal io.Reader over a byte slice, avoiding a
// dependency on bytes.NewReader for this one-shot use.
type bytesReader struct {
	data []byte
	off  int
}

func (r *bytesReader) Read(b []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(b, r.data[r.off:])
	r.off += n
	return n, nil
}
