package jwtverifier

import "errors"

// Verification failure modes surfaced by Verifier and JWKS cache. Callers
// inspect them with errors.Is to decide between 401 (bad token) vs 503
// (JWKS unavailable).
var (
	ErrInvalidSignature = errors.New("jwtverifier: invalid signature")
	ErrExpired          = errors.New("jwtverifier: token expired")
	ErrNotYetValid      = errors.New("jwtverifier: token not yet valid")
	ErrWrongAudience    = errors.New("jwtverifier: wrong audience")
	ErrWrongIssuer      = errors.New("jwtverifier: wrong issuer")
	ErrRevoked          = errors.New("jwtverifier: token revoked")
	ErrMalformed        = errors.New("jwtverifier: malformed token")
	ErrJWKSUnavailable  = errors.New("jwtverifier: jwks unavailable")
)
