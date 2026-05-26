package core

// TLSInfo captures TLS-level context available at the connection stage,
// before the TLS handshake to the upstream is completed.
//
// Populated by the service entry point that invokes the connection-stage
// pipeline (agent MITM after SNI peek, compliance-proxy on CONNECT,
// ai-gateway on HTTP entry middleware).
type TLSInfo struct {
	// SNI is the server_name extension value from the ClientHello.
	SNI string `json:"sni,omitempty"`

	// ClientCertFingerprint is the SHA-256 fingerprint of the presented
	// client certificate, formatted "sha256:<hex>". Empty string when
	// mTLS is not in use at this ingress.
	ClientCertFingerprint string `json:"clientCertFingerprint,omitempty"`
}
