package tlsbump

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// captureALPNOffer starts a TLS listener that records the ALPN list the CLIENT
// offered and then aborts the handshake. The handshake outcome is irrelevant:
// the subject is what we put in the ClientHello, which is where the defect lives
// and the only place it is observable — once the server has chosen, the offer is
// gone.
func captureALPNOffer(t *testing.T) (addr string, offered func() []string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	got := make(chan []string, 4)
	go func() {
		for {
			conn, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = tls.Server(c, &tls.Config{
					GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
						protos := append([]string(nil), hi.SupportedProtos...)
						select {
						case got <- protos:
						default:
						}
						// Abort: we have what we came for, and returning a real
						// config would need a cert this test has no reason to own.
						return nil, errALPNCaptured
					},
				}).Handshake()
			}(conn)
		}
	}()
	return ln.Addr().String(), func() []string {
		select {
		case p := <-got:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no ClientHello captured within 5s")
			return nil
		}
	}, func() { _ = ln.Close() }
}

var errALPNCaptured = &alpnCapturedError{}

type alpnCapturedError struct{}

func (*alpnCapturedError) Error() string { return "alpn captured; handshake intentionally aborted" }

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestFingerprintDialer_H1DialOffersHTTP11Only is the regression guard for the
// defect that made a bumped host 502 permanently.
//
// The dispatcher caches the negotiated protocol per authority and never
// invalidates it, so ONE http/1.1 negotiation pins that authority to the STDLIB
// transport for the life of the process. That transport parses HTTP/1.x — but it
// shared the [h2, http/1.1] dial with the h2 transport, so an upstream that
// prefers h2 (api.openai.com does) handed it an h2 connection and it read the
// peer's SETTINGS frame as a status line:
// `malformed HTTP response "\x00\x00\x12\x04..."` — a 502 to the client, for
// every subsequent request to that host.
//
// Measured before the fix: a poisoned proxy process answered 502 on three
// identical requests that a freshly restarted process answered 200 three times.
//
// The no-raw-ClientHello path is deliberate: overrideALPN only rewrites the
// replayed fingerprint spec, so this is the path a spec rewrite alone would
// leave uncovered.
func TestFingerprintDialer_H1DialOffersHTTP11Only(t *testing.T) {
	addr, offered, stop := captureALPNOffer(t)
	defer stop()

	fd := &fingerprintDialer{dialer: &net.Dialer{Timeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if conn, err := fd.dialH1(ctx, "tcp", addr); err == nil {
		_ = conn.Close()
	}

	protos := offered()
	if contains(protos, "h2") {
		t.Fatalf("dialH1 offered h2 (%v) — the stdlib HTTP/1.x transport would then "+
			"read the peer's h2 frames as a status line and 502", protos)
	}
	if !contains(protos, "http/1.1") {
		t.Errorf("dialH1 offered %v, want http/1.1 present", protos)
	}
}

// TestFingerprintDialer_H2DialStillOffersH2 is the other half: narrowing the h1
// path must not narrow the h2 probe, or every authority would fall back to
// http/1.1 and the h2 dispatcher would become dead code.
func TestFingerprintDialer_H2DialStillOffersH2(t *testing.T) {
	addr, offered, stop := captureALPNOffer(t)
	defer stop()

	fd := &fingerprintDialer{dialer: &net.Dialer{Timeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if conn, err := fd.dial(ctx, "tcp", addr); err == nil {
		_ = conn.Close()
	}

	protos := offered()
	if !contains(protos, "h2") {
		t.Errorf("dial offered %v, want h2 present — the h2 dispatcher relies on it", protos)
	}
}
