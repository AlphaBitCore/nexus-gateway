// Package connect provides the CONNECT tunnel establishment helper.
// It hijacks the HTTP connection after sending "200 Connection Established"
// and returns the raw net.Conn for TLS interception.
package connect

import (
	"fmt"
	"net"
	"net/http"

	iconn "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/bufconn"
)

// EstablishTunnel sends a "200 Connection Established" response to the client
// and hijacks the connection, returning the raw net.Conn for TLS interception.
// If the HTTP server's bufio.Reader has pre-read bytes beyond the CONNECT
// request line, they are preserved by wrapping the connection with a BufConn
// that drains the buffered bytes before reading from the socket.
func EstablishTunnel(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "HTTP server does not support hijacking", http.StatusInternalServerError)
		return nil, fmt.Errorf("ResponseWriter does not implement http.Hijacker")
	}

	rawConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "failed to hijack connection", http.StatusInternalServerError)
		return nil, fmt.Errorf("hijack connection: %w", err)
	}

	// Write the CONNECT success response directly to the hijacked connection.
	// After Hijack(), the ResponseWriter is no longer usable.
	connectResponse := "HTTP/1.1 200 Connection Established\r\n\r\n"
	if _, err := bufrw.WriteString(connectResponse); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("write 200 Connection Established: %w", err)
	}
	if err := bufrw.Flush(); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("flush 200 Connection Established: %w", err)
	}

	// Preserve any bytes the HTTP server's bufio.Reader already consumed
	// from the TCP stream beyond the CONNECT request line. Without this,
	// a pipelining client's TLS ClientHello would be silently truncated.
	var result = rawConn
	if bufrw.Reader.Buffered() > 0 {
		peeked, _ := bufrw.Peek(bufrw.Reader.Buffered())
		buf := make([]byte, len(peeked))
		copy(buf, peeked)
		result = iconn.New(rawConn, buf)
	}

	return result, nil
}
