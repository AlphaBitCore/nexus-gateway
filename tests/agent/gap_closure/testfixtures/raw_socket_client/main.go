// fixtures/gap1-raw-socket/main.go
//
// Intentionally naive outbound HTTPS client — deliberately avoids any Nexus
// package and any system proxy environment variable so the pf rdr rule is the
// only possible interception path. Used by TestGap1RawSocket in the
// tests/agent/gap_closure package.
//
// Usage:
//
//	nexus-gap1-client --trace-id <id> [--host <host>] [--timeout <dur>]
//
// The binary embeds the trace ID as an HTTP header X-Nexus-Request-Id so the
// gap_closure test harness can locate the resulting traffic_event row.
// Upstream response status is irrelevant — the pf path intercepts regardless.
//
// This binary MUST NOT import any Nexus package. It is an adversarial test
// fixture: a naive process that knows nothing about the proxy, as if it were
// a third-party app on the user's Mac.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	traceID := flag.String("trace-id", "", "Unique trace ID embedded as X-Nexus-Request-Id header (required)")
	host := flag.String("host", "api.openai.com", "Target host (TCP dial, port 443)")
	timeout := flag.Duration("timeout", 10*time.Second, "Total dial + request timeout")
	flag.Parse()

	if *traceID == "" {
		fmt.Fprintln(os.Stderr, "gap1-client: --trace-id is required")
		os.Exit(1)
	}

	if err := run(*host, *traceID, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "gap1-client: %v\n", err)
		// Non-zero only on dial failure; upstream HTTP errors are expected
		// (proxy may return 400, upstream may return various codes).
		os.Exit(1)
	}
}

func run(host, traceID string, timeout time.Duration) error {
	addr := net.JoinHostPort(host, "443")

	// Explicitly avoid http.ProxyFromEnvironment — use a raw net.Dial so pf
	// is the only path the kernel can intercept.
	dialer := &net.Dialer{Timeout: timeout}
	rawConn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("TCP dial to %s failed: %w", addr, err)
	}
	defer rawConn.Close()

	// Wrap in TLS. InsecureSkipVerify is intentional here: the pf listener
	// performs MITM so the certificate we receive is the Nexus MITM cert,
	// not the upstream's. We do not validate it — the goal is interception
	// proof, not upstream TLS correctness.
	tlsCfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, //nolint:gosec // test fixture — intentional
		MinVersion:         tls.VersionTLS12,
	}
	tlsConn := tls.Client(rawConn, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck

	if err := tlsConn.Handshake(); err != nil {
		// A TLS handshake error is still useful proof that the pf rule fired —
		// the daemon may terminate TLS (expected MITM) even if it returns an
		// error response. Log and continue rather than exit.
		fmt.Fprintf(os.Stderr, "gap1-client: TLS handshake warning (may be expected MITM): %v\n", err)
		// Attempt a plain HTTP/1.1 GET through the raw connection; the listener
		// may have already performed the handshake on its side.
	}

	// Send a minimal HTTP/1.1 GET with the trace header so the normalizer can
	// pick up the trace ID from the request body or header.
	req := fmt.Sprintf(
		"GET /v1/models HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"X-Nexus-Request-Id: %s\r\n"+
			"User-Agent: nexus-gap1-test-client/1.0\r\n"+
			"Accept: application/json\r\n"+
			"Connection: close\r\n"+
			"\r\n",
		host, traceID,
	)

	var writer io.Writer = tlsConn
	if _, writeErr := fmt.Fprint(writer, req); writeErr != nil {
		return fmt.Errorf("write HTTP request: %w", writeErr)
	}

	// Read response — consume enough to confirm the proxy returned something.
	buf := make([]byte, 4096)
	n, readErr := writer.(io.Reader).Read(buf)
	if n > 0 {
		fmt.Printf("gap1-client: received %d bytes from %s (trace-id=%s)\n", n, host, traceID)
		// Print the first line of the response for diagnostics.
		respLine := string(buf[:n])
		if nl := len(respLine); nl > 200 {
			respLine = respLine[:200]
		}
		fmt.Printf("gap1-client: response preview: %s\n", respLine[:min(80, len(respLine))])
	}
	if readErr != nil && readErr != io.EOF {
		// Non-EOF read errors after the handshake are expected — the MITM
		// listener may close the connection after writing its response.
		fmt.Fprintf(os.Stderr, "gap1-client: read (expected after MITM): %v\n", readErr)
	}

	fmt.Printf("gap1-client: done (trace-id=%s)\n", traceID)
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
