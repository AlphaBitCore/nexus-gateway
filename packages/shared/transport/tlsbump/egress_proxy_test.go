package tlsbump

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// startEcho starts a TCP echo server and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// startSOCKS5 starts a minimal no-auth SOCKS5 CONNECT proxy that relays to the
// requested target. Returns its address.
func startSOCKS5(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5(c)
		}
	}()
	return ln.Addr().String()
}

func handleSOCKS5(c net.Conn) {
	defer c.Close() //nolint:errcheck
	greet := make([]byte, 2)
	if _, err := io.ReadFull(c, greet); err != nil {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, int(greet[1]))); err != nil {
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil { // no-auth
		return
	}
	hdr := make([]byte, 4) // ver, cmd, rsv, atyp
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	var host string
	switch hdr[3] {
	case 0x01:
		b := make([]byte, 4)
		_, _ = io.ReadFull(c, b)
		host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		_, _ = io.ReadFull(c, l)
		d := make([]byte, int(l[0]))
		_, _ = io.ReadFull(c, d)
		host = string(d)
	case 0x04:
		b := make([]byte, 16)
		_, _ = io.ReadFull(c, b)
		host = net.IP(b).String()
	default:
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(pb[0])<<8|int(pb[1])))
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()                                                 //nolint:errcheck
	_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // success
	go func() { _, _ = io.Copy(up, c) }()
	_, _ = io.Copy(c, up)
}

// startHTTPConnect starts a minimal HTTP CONNECT proxy.
func startHTTPConnect(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("connect listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close() //nolint:errcheck
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
					return
				}
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close() //nolint:errcheck
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().String()
}

// roundTrip writes a probe through conn and asserts the echo comes back —
// proving the tunnel actually reaches the target.
func roundTrip(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close() //nolint:errcheck
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	const probe = "nexus-egress-probe"
	if _, err := conn.Write([]byte(probe)); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, len(probe))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo through tunnel: %v", err)
	}
	if string(got) != probe {
		t.Fatalf("echo = %q, want %q — tunnel did not reach the target", got, probe)
	}
}

func TestDialUpstreamTCP_Direct(t *testing.T) {
	target := startEcho(t)
	conn, err := dialUpstreamTCP(context.Background(), "tcp", target, &net.Dialer{}, nil)
	if err != nil {
		t.Fatalf("direct dial: %v", err)
	}
	roundTrip(t, conn)
}

func TestDialUpstreamTCP_SOCKS5(t *testing.T) {
	target := startEcho(t)
	u, _ := url.Parse("socks5://" + startSOCKS5(t))
	conn, err := dialUpstreamTCP(context.Background(), "tcp", target, &net.Dialer{}, u)
	if err != nil {
		t.Fatalf("socks5 dial: %v", err)
	}
	roundTrip(t, conn)
}

func TestDialUpstreamTCP_HTTPConnect(t *testing.T) {
	target := startEcho(t)
	u, _ := url.Parse("http://" + startHTTPConnect(t))
	conn, err := dialUpstreamTCP(context.Background(), "tcp", target, &net.Dialer{}, u)
	if err != nil {
		t.Fatalf("http connect dial: %v", err)
	}
	roundTrip(t, conn)
}

func TestDialUpstreamTCP_UnsupportedScheme(t *testing.T) {
	u, _ := url.Parse("ftp://127.0.0.1:21")
	if _, err := dialUpstreamTCP(context.Background(), "tcp", "127.0.0.1:1", &net.Dialer{}, u); err == nil {
		t.Fatal("want error for unsupported scheme, got nil")
	}
}

func TestDialUpstreamTCP_HTTPConnectRefused(t *testing.T) {
	// Proxy points at a dead target -> CONNECT returns 502 -> dial errors.
	u, _ := url.Parse("http://" + startHTTPConnect(t))
	// 127.0.0.1:1 is reserved/unbound in practice; the proxy's Dial fails -> 502.
	if _, err := dialUpstreamTCP(context.Background(), "tcp", "127.0.0.1:1", &net.Dialer{}, u); err == nil {
		t.Fatal("want CONNECT error to dead target, got nil")
	}
}

func TestParseEgressProxy(t *testing.T) {
	cases := []struct {
		in      string
		wantNil bool
		wantErr bool
		scheme  string
	}{
		{"", true, false, ""},
		{"   ", true, false, ""},
		{"socks5://127.0.0.1:10808", false, false, "socks5"},
		{"socks5h://user:pass@127.0.0.1:1080", false, false, "socks5h"},
		{"http://127.0.0.1:8080", false, false, "http"},
		{"https://proxy.local:443", false, false, "https"},
		{"ftp://127.0.0.1:21", false, true, ""},
		{"socks5://", false, true, ""}, // missing host
		{"://bad", false, true, ""},
	}
	for _, tc := range cases {
		u, err := ParseEgressProxy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got nil", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if tc.wantNil {
			if u != nil {
				t.Errorf("%q: want nil URL, got %v", tc.in, u)
			}
			continue
		}
		if u == nil || u.Scheme != tc.scheme {
			t.Errorf("%q: scheme = %v, want %q", tc.in, u, tc.scheme)
		}
	}
}
