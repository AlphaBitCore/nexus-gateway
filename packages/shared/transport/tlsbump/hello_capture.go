package tlsbump

import "net"

// helloCapture wraps a net.Conn and records every byte read from it until a
// complete TLS ClientHello record is buffered. It is installed between the
// raw client connection and tls.Server so BumpConnection can extract the
// client's exact fingerprint and replay it to the upstream via uTLS.
type helloCapture struct {
	net.Conn
	buf  []byte
	done bool
}

func (h *helloCapture) Read(p []byte) (int, error) {
	n, err := h.Conn.Read(p)
	if !h.done && n > 0 {
		h.buf = append(h.buf, p[:n]...)
		if hasCompleteClientHello(h.buf) {
			h.done = true
		}
	}
	return n, err
}

// hasCompleteClientHello returns true when b contains at least one full TLS
// Handshake record (type 0x16). The TLS record header is 5 bytes:
// type(1) + version(2) + length(2); the total record is 5 + length bytes.
func hasCompleteClientHello(b []byte) bool {
	if len(b) < 5 || b[0] != 0x16 {
		return false
	}
	recordLen := int(b[3])<<8 | int(b[4])
	return len(b) >= 5+recordLen
}
