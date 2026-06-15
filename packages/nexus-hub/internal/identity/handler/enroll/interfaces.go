// Package enroll defines the HTTP handler for agent enrollment
// (POST /api/internal/things/enroll).
//
// This file declares the minimal interfaces the handler depends on so that
// tests can substitute lightweight stubs without spinning up a real CA,
// database, or fleet manager.
package enroll

import (
	"context"
	"crypto/rsa"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// CertAuthority is the methods the enrollment handler calls on the agent CA.
// *agentca.CA satisfies this interface. SignAttestationCSR handles the
// Ed25519-only attestation-key enrollment flow that the compliance-proxy uses
// to verify signed attestation headers.
type CertAuthority interface {
	SignAttestationCSR(csrPEM string, subjectCN string) (*agentca.CertResult, error)
}

// FleetManager is the subset of *manager.Manager the enrollment handler uses.
// *manager.Manager satisfies this interface.
type FleetManager interface {
	RegisterThing(ctx context.Context, req manager.RegisterRequest) (*manager.RegisterResponse, error)
	ComputeAndStoreTrustLevel(ctx context.Context, thingID, thingStatus, minVersion string) int
	Store() *store.Store
}

// EnrollmentSvc is the subset of *enrollment.Service the enrollment handler uses.
// *enrollment.Service satisfies this interface.
//
// ConsumeToken atomically validates + single-use-consumes the token:
// the validate-then-mark two-step that preceded it let two concurrent requests
// both pass a SELECT and both enroll. LinkThing records the minted thing id on
// the already-consumed token row (best-effort).
type EnrollmentSvc interface {
	ConsumeToken(ctx context.Context, rawToken string) (*enrollment.Token, error)
	LinkThing(ctx context.Context, tokenID, thingID string) error
}

// JWKSKeyGetter is the one method the enrollment handler calls on the JWKS cache
// to resolve a JWT signing key by kid. *jwks.Cache satisfies this interface.
type JWKSKeyGetter interface {
	Get(kid string) (*rsa.PublicKey, error)
}
