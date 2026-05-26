package oauth

import (
	"errors"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/pkce"
)

// ErrPKCEMismatch is returned when the PKCE verifier does not match the stored challenge.
var ErrPKCEMismatch = errors.New("pkce: verifier does not match challenge")

// VerifyPKCE implements RFC 7636 S256-only PKCE verification. The
// "plain" method is rejected per our security policy. Length bounds
// and the constant-time S256 compare live in shared/pkce.
func VerifyPKCE(verifier, challenge, method string) error {
	if method != "S256" {
		return errors.New("pkce: unsupported method " + method)
	}
	if n := len(verifier); n < pkce.VerifierMinLen || n > pkce.VerifierMaxLen {
		return errors.New("pkce: verifier length out of range")
	}
	if !pkce.VerifyS256(verifier, challenge) {
		return ErrPKCEMismatch
	}
	return nil
}
