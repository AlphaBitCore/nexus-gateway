package tlsbump

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"

	utls "github.com/refraction-networking/utls"
)

// pskExtensionTypeID is the TLS extension type for pre_shared_key (RFC 8446 §4.2.11).
// PSK binders are transcript hashes bound to the original session and cannot
// be replayed to a different upstream server.
const pskExtensionTypeID uint16 = 41

// earlyDataExtensionTypeID is the TLS extension type for early_data (RFC 8446 §4.2.10).
// It accompanies PSK and must be stripped alongside it.
const earlyDataExtensionTypeID uint16 = 42

// dialWithFingerprint opens a TCP connection to addr and completes a TLS
// handshake that mirrors the client's original fingerprint. When rawHello is
// non-empty it is parsed with the uTLS Fingerprinter and replayed to the
// upstream after sanitising session-specific state (session tickets, PSK, and
// early-data extensions). The ALPN list is replaced with ["http/1.1"] so the
// upstream negotiates HTTP/1.1 (Go's transport cannot upgrade a *utls.UConn
// to HTTP/2). On any parse or handshake failure the function falls back to
// HelloChrome_Auto.
func dialWithFingerprint(ctx context.Context, network, addr string, rawHello []byte, dialer *net.Dialer) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(addr)

	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	helloID := utls.HelloChrome_Auto
	var spec *utls.ClientHelloSpec

	if len(rawHello) > 0 {
		fp := &utls.Fingerprinter{AllowBluntMimicry: true}
		if s, fErr := fp.FingerprintClientHello(rawHello); fErr == nil {
			overrideALPN(s, []string{"http/1.1"})
			sanitizeForReplay(s)
			spec = s
			helloID = utls.HelloCustom
		}
	}

	uconn := utls.UClient(conn, &utls.Config{ServerName: host}, helloID)
	if spec != nil {
		if err := uconn.ApplyPreset(spec); err != nil {
			// ApplyPreset failed — fall back to Chrome fingerprint.
			_ = uconn.Close()
			conn2, dialErr := dialer.DialContext(ctx, network, addr)
			if dialErr != nil {
				return nil, fmt.Errorf("dial fallback %s: %w", addr, dialErr)
			}
			uconn = utls.UClient(conn2, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
		}
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = uconn.Close()
		return nil, fmt.Errorf("utls handshake to %s: %w", addr, err)
	}
	return uconn, nil
}

// ProbeUpstreamCert opens a TLS connection to dstHost:dstPort using the
// client's exact TLS fingerprint (peeked ClientHello replayed via uTLS),
// captures the upstream's leaf certificate, then closes. Used by agent's
// NE bridge to probe the cert before TLS-bumping the client side, so the
// agent's device CA can mint a leaf with the upstream's CN + SAN list.
//
// Why this matters vs a vanilla tls.Dial probe: hosts behind strict
// anti-bot (Cursor's api2.cursor.sh, some Cloudflare-fronted endpoints)
// reject vanilla Go TLS fingerprints with "first record does not look
// like a TLS handshake" because Go's standard ClientHello looks nothing
// like Chrome / Cursor's Electron / Anthropic SDK's httpx. Replaying
// the client's actual fingerprint makes the probe indistinguishable
// from the original outbound connection — the agent's bumped path
// then captures method/path/body + runs the hook chain end-to-end.
//
// peekedHello SHOULD be the raw ClientHello bytes the bridge ingress
// peeked from the client side. When empty/nil, falls back to
// HelloChrome_Auto (still much better than vanilla Go).
func ProbeUpstreamCert(ctx context.Context, dstHost string, dstPort int, peekedHello []byte, dialer *net.Dialer) (*x509.Certificate, error) {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	addr := net.JoinHostPort(dstHost, fmt.Sprintf("%d", dstPort))
	conn, err := dialWithFingerprint(ctx, "tcp", addr, peekedHello, dialer)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck
	uconn, ok := conn.(*utls.UConn)
	if !ok {
		return nil, fmt.Errorf("ProbeUpstreamCert: dialWithFingerprint returned non-uTLS conn type %T", conn)
	}
	certs := uconn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("ProbeUpstreamCert: no peer certificates from %s", dstHost)
	}
	return certs[0], nil
}

// sanitizeForReplay removes or clears session-specific TLS extensions that
// cannot be replayed to a different upstream server:
//
//   - SessionTicketExtension: ticket data cleared so the client advertises
//     support for resumption but does not attempt it.
//   - PSK (pre_shared_key, type 41) and early_data (type 42): stripped
//     entirely. PSK binders are transcript hashes bound to the original
//     session; replaying them to a different server causes a decrypt_error
//     fatal alert.
//   - GenericExtension with the above type IDs: also stripped (handles
//     extensions uTLS wraps as raw bytes via AllowBluntMimicry).
//
// Extension type IDs that remain in the list preserve the JA3 extension-ID
// fingerprint as much as the TLS spec allows.
func sanitizeForReplay(spec *utls.ClientHelloSpec) {
	out := spec.Extensions[:0]
	for _, ext := range spec.Extensions {
		switch e := ext.(type) {
		case *utls.SessionTicketExtension:
			// Keep extension type ID in the list but clear the ticket bytes.
			out = append(out, &utls.SessionTicketExtension{Session: nil})
			_ = e
		case *utls.FakePreSharedKeyExtension:
			// PSK cannot be replayed — drop.
		case *utls.UtlsPreSharedKeyExtension:
			// PSK cannot be replayed — drop.
		case *utls.GenericExtension:
			// AllowBluntMimicry may wrap PSK or early_data as raw bytes.
			if e.Id != pskExtensionTypeID && e.Id != earlyDataExtensionTypeID {
				out = append(out, ext)
			}
		default:
			out = append(out, ext)
		}
	}
	spec.Extensions = out
}

// overrideALPN replaces the ALPN protocol list in spec with protocols. If
// spec contains no ALPN extension the function is a no-op — adding a new
// extension would change the JA3 extension-type list and break the
// fingerprint match.
func overrideALPN(spec *utls.ClientHelloSpec, protocols []string) {
	for _, ext := range spec.Extensions {
		if alpnExt, ok := ext.(*utls.ALPNExtension); ok {
			alpnExt.AlpnProtocols = protocols
			return
		}
	}
}
