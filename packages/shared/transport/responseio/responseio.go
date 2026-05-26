// Package responseio parses an HTTP response from a buffered reader, lets the
// caller mutate response headers (e.g. inject x-nexus-* markers), filters
// hop-by-hop headers, and streams the body to a writer without buffering.
//
// It is the shared mechanism used by compliance-proxy's transparent proxy
// path and by the agent's MITM relay so both can inject response markers.
package responseio

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HeaderHook receives the parsed response BEFORE its headers are written to
// the destination. The hook may freely mutate resp.Header. The hook MUST NOT
// read or close resp.Body — body streaming is owned by Copy.
type HeaderHook func(*http.Response)

// hopByHopHeaders are stripped before the response is forwarded
// (RFC 7230 §6.1). Transfer-Encoding is rebuilt by resp.Write when the body
// has unknown length.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Transfer-Encoding",
	"TE",
	"Trailer",
	"Upgrade",
	"Proxy-Authorization",
	"Proxy-Authenticate",
}

// Copy parses an HTTP response from src, invokes hook (if non-nil) to allow
// header mutation, writes a properly framed response to dst (status line +
// filtered headers + body), then returns the first non-EOF error.
//
// Framing is delegated to (*http.Response).Write: it emits Content-Length when
// ContentLength >= 0, or re-adds Transfer-Encoding: chunked otherwise, so
// keep-alive client connections see correct message boundaries.
func Copy(dst io.Writer, src *bufio.Reader, hook HeaderHook) error {
	resp, err := http.ReadResponse(src, nil)
	if err != nil {
		return fmt.Errorf("responseio: read response: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if hook != nil {
		hook(resp)
	}

	// RFC 7230 §6.1: strip any header names listed in the Connection header
	// (dynamic hop-by-hop headers) before deleting Connection itself.
	// Header.Values iterates all lines of the same header; Get returns only the first.
	for _, line := range resp.Header.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			if n := strings.TrimSpace(name); n != "" {
				resp.Header.Del(n)
			}
		}
	}

	// Strip the static set of hop-by-hop headers (includes Connection itself).
	for _, h := range hopByHopHeaders {
		resp.Header.Del(h)
	}

	// resp.Write writes the status line (using resp.Status, which preserves
	// the upstream's reason phrase verbatim), the filtered headers, and the
	// body with correct framing (Content-Length or Transfer-Encoding: chunked).
	if err := resp.Write(dst); err != nil {
		return fmt.Errorf("responseio: write response: %w", err)
	}
	return nil
}
