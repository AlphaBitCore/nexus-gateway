package authserver

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	store "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// Deps carries external collaborators required by the authserver package.
// Short-lived in-memory stores (pending-authz, auth codes, device bindings)
// are constructed inside Mount and deliberately kept out of Deps so callers do
// not leak the auth server's internal state into their wiring.
type Deps struct {
	// DB is the pgx pool backing every DB-resident store (clients, users,
	// refresh tokens, federated identities, IdPs).
	DB *pgxpool.Pool
	// Keystore resolves kid → RSA key for JWKS publication and introspection.
	Keystore *token.Keystore
	// Signer is the RS256 signer used to mint access tokens.
	Signer *token.Signer
	// Logger is shared across handlers; nil loggers fall back to io.Discard
	// inside each handler rather than at wiring time.
	Logger *slog.Logger
	// Issuer is the canonical iss claim advertised by discovery and stamped
	// into every issued access token (e.g. "https://cp.nexus.ai").
	Issuer string
	// AgentLookup resolves an mTLS peer serial to a device row. When nil the
	// /oauth/device-binding route is still registered but will 401 because
	// the middleware chain is absent; production wires *store.DB here.
	AgentLookup middleware.ThingNodeLookup
	// Revocation records token revocations and fans them out over MQ. When
	// nil the /oauth/revoke route is skipped with a warn log (mirrors the
	// AgentLookup pattern) so the service still boots for test harnesses
	// that do not wire the revocation pipeline.
	Revocation *revocation.Service
	// Audit emits AdminAuditLog rows for security-relevant events (e.g.
	// admin.login.failed / admin.login.succeeded). nil = no audit emission
	// (test harnesses); production main.go wires the same auditWriter
	// used by the admin handlers.
	Audit *audit.Writer
	// AuthCodes is the in-memory authorization code store shared between the
	// authserver login flow (which creates codes) and the sso-enroll endpoint
	// (which consumes them). When nil, Mount creates its own internal store.
	AuthCodes *store.AuthCodeStore
}
