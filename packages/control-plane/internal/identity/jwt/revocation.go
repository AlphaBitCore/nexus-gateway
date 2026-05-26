package jwtverifier

import "context"

// RevocationChecker tells the Verifier whether a token whose signature and
// expiry already verified should still be rejected. Implementations may
// consult an in-memory deny list, an MQ-fed set, or a remote service.
type RevocationChecker interface {
	IsRevoked(ctx context.Context, claims *Claims) (bool, error)
}

// AlwaysAllow is a no-op RevocationChecker that never rejects a token.
// Callers MUST opt into it explicitly in wiring — it ships as an opt-in so
// there is no silent fallback when a real checker is missing from the deps.
type AlwaysAllow struct{}

// IsRevoked always returns (false, nil).
func (AlwaysAllow) IsRevoked(context.Context, *Claims) (bool, error) { return false, nil }
