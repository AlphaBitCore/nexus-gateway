package store

import (
	"crypto/rand"
	"encoding/base64"
)

// RandomOpaqueToken returns a base64url-encoded random token backed by n bytes
// of cryptographic entropy. Callers use this for authorization codes,
// authctx handles, and similar short-lived opaque identifiers. The encoding
// is RawURLEncoding so the output is URL-safe without padding.
//
// A crypto/rand failure is treated as a fatal environmental error; the
// function panics so callers cannot silently mint predictable tokens.
func RandomOpaqueToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
