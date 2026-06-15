package tlsbump

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// dialUpstreamTCP opens a TCP connection to addr, optionally routing it
// through an egress proxy.
//
// When proxyURL is nil it dials addr directly via dialer (preserving the
// SO_MARK Control hook). When proxyURL is set it dials the PROXY via dialer —
// so the SO_MARK still applies to the proxy hop and the agent's own egress
// still escapes its iptables REDIRECT — and then tunnels to addr through it:
//
//   - socks5 / socks5h: SOCKS5 CONNECT (golang.org/x/net/proxy).
//   - http / https:     HTTP CONNECT.
//
// The returned conn carries plaintext bytes to addr; the caller layers uTLS on
// top, so the proxy only ever sees the CONNECT target host:port, never the
// inner TLS. This lets the agent forward intercepted AI traffic through a
// local SOCKS/HTTP proxy (e.g. a corporate egress or a censorship-circumvention
// client) while still MITM-inspecting it.
func dialUpstreamTCP(ctx context.Context, network, addr string, dialer *net.Dialer, proxyURL *url.URL) (net.Conn, error) {
	if proxyURL == nil {
		return dialer.DialContext(ctx, network, addr)
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "socks5", "socks5h":
		var auth *xproxy.Auth
		if u := proxyURL.User; u != nil {
			pw, _ := u.Password()
			auth = &xproxy.Auth{User: u.Username(), Password: pw}
		}
		// forward = our SO_MARK dialer, so the hop to the proxy is marked and
		// escapes the agent's own REDIRECT.
		sd, err := xproxy.SOCKS5("tcp", proxyURL.Host, auth, dialer)
		if err != nil {
			return nil, fmt.Errorf("egress socks5 %s: %w", proxyURL.Host, err)
		}
		cd, ok := sd.(xproxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("egress socks5 %s: dialer is not context-aware", proxyURL.Host)
		}
		conn, err := cd.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("egress socks5 %s -> %s: %w", proxyURL.Host, addr, err)
		}
		return conn, nil
	case "http", "https":
		return dialHTTPConnect(ctx, network, addr, dialer, proxyURL)
	default:
		return nil, fmt.Errorf("egress proxy: unsupported scheme %q (want socks5:// or http://)", proxyURL.Scheme)
	}
}

// dialHTTPConnect dials the HTTP proxy via dialer and issues a CONNECT to addr,
// returning the tunneled connection on a 200 response.
func dialHTTPConnect(ctx context.Context, network, addr string, dialer *net.Dialer, proxyURL *url.URL) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, network, proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("egress http proxy dial %s: %w", proxyURL.Host, err)
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u := proxyURL.User; u != nil {
		pw, _ := u.Password()
		req.SetBasicAuth(u.Username(), pw)
		req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
		req.Header.Del("Authorization")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("egress http CONNECT write %s: %w", addr, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("egress http CONNECT read %s: %w", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("egress http CONNECT to %s: proxy returned %s", addr, resp.Status)
	}
	// A conformant proxy sends nothing after the 200 until we speak; if it
	// buffered tunnel bytes into the reader, returning the bare conn would lose
	// them, so fail loud rather than silently corrupt the stream.
	if br.Buffered() > 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("egress http CONNECT to %s: proxy sent %d unexpected bytes before tunnel", addr, br.Buffered())
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// ParseEgressProxy validates and parses an egress proxy URL from config.
// Empty input returns (nil, nil) — no proxy. Only socks5/socks5h/http/https
// schemes with a host are accepted.
func ParseEgressProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse egress proxy %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h", "http", "https":
	default:
		return nil, fmt.Errorf("egress proxy %q: unsupported scheme %q (want socks5:// or http://)", raw, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("egress proxy %q: missing host", raw)
	}
	return u, nil
}
