package authserver

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/idp"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// inMemoryStoreTTL is the lifetime applied to the three in-memory stores
// Mount creates. Five minutes comfortably outlasts an interactive login
// while keeping abandoned handles from lingering in process memory.
const inMemoryStoreTTL = 5 * time.Minute

// StoreFactory builds the six DB-backed stores Mount consumes. Production
// wiring uses the *pgxpool.Pool-backed defaultStoreFactory (see Mount); test
// harnesses inject a fake factory through MountWithFactory so they can drive
// IdPStore.GetLocal — and therefore the post-success branch at the bottom of
// Mount — without standing up a Postgres connection. Mirrors the PgxPool
// seam in packages/control-plane/internal/authserver/revocation/store.go.
type StoreFactory interface {
	Clients() *store.ClientStore
	Users() *store.UserStore
	IdPs() *store.IdPStore
	Refresh() *store.RefreshStore
	Assignments() *store.AssignmentStore
	Federated() *store.FederatedStore
}

// defaultStoreFactory constructs each store on demand from a *pgxpool.Pool.
// One instance per Mount call; the pool itself is shared by the constructed
// stores so connection accounting stays correct.
type defaultStoreFactory struct{ pool *pgxpool.Pool }

func (f defaultStoreFactory) Clients() *store.ClientStore  { return store.NewClientStore(f.pool) }
func (f defaultStoreFactory) Users() *store.UserStore      { return store.NewUserStore(f.pool) }
func (f defaultStoreFactory) IdPs() *store.IdPStore        { return store.NewIdPStore(f.pool) }
func (f defaultStoreFactory) Refresh() *store.RefreshStore { return store.NewRefreshStore(f.pool) }
func (f defaultStoreFactory) Assignments() *store.AssignmentStore {
	return store.NewAssignmentStore(f.pool)
}
func (f defaultStoreFactory) Federated() *store.FederatedStore {
	return store.NewFederatedStore(f.pool)
}

// Mounted holds references to the wired auth-server collaborators that
// MountWithFactory chooses to expose. Today only RefreshHelper is surfaced
// because the refresh-token replay-detection closure (Mount's `if d.Revocation
// != nil` block) is otherwise unreachable from a test without driving a full
// /oauth/token round-trip against a live Postgres. Adding more fields requires
// a deliberate design decision — keep the surface area minimal.
type Mounted struct {
	// RefreshHelper is the token.RefreshHelper Mount wired into the
	// /oauth/token handler. Its ReplayHook field is set when d.Revocation is
	// non-nil; tests assert on the side effects of invoking that closure
	// without driving the full OAuth flow.
	RefreshHelper *token.RefreshHelper
}

// Mount registers the OAuth/OIDC and SPA-facing JSON login routes served by
// the control plane. It constructs the short-lived in-memory stores
// (pending authz, auth codes, device bindings) internally; callers only
// supply the DB-backed stores and signing material via Deps.
//
// Local-IdP resolution is best-effort: when the IdentityProvider row is
// missing (e.g. the seed has not run yet) or the lookup fails, Mount still
// registers the OAuth endpoints but skips /authserver/* so the service
// starts without a hard dependency on the seed ordering.
func Mount(e *echo.Echo, d Deps) {
	if d.DB == nil {
		// Pass nil factory; mountCore early-returns at the same point as before
		// once it sees no factory + no DB.
		mountCore(e, d, nil)
		return
	}
	mountCore(e, d, defaultStoreFactory{pool: d.DB})
}

// MountWithFactory is the test-only entry point that lets a harness supply a
// non-pgxpool StoreFactory. Production callers must use Mount; the public
// Mount signature (and therefore cmd/control-plane/main.go's wiring) is
// untouched. Returns a *Mounted exposing the internal RefreshHelper so tests
// can invoke the replay-hook closure directly.
func MountWithFactory(e *echo.Echo, d Deps, f StoreFactory) *Mounted {
	return mountCore(e, d, f)
}

// mountCore is the shared body of Mount and MountWithFactory. Returning
// *Mounted lets MountWithFactory expose the wired RefreshHelper to tests;
// Mount discards the return value.
func mountCore(e *echo.Echo, d Deps, f StoreFactory) *Mounted {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// JWKS + discovery do not require the DB and can always be published.
	if d.Keystore != nil {
		e.GET("/.well-known/jwks.json", oauth.JWKSHandler(d.Keystore))
	}
	e.GET("/.well-known/openid-configuration", oauth.DiscoveryHandler(d.Issuer))

	// Without a factory we cannot load clients, users, refresh tokens, or
	// IdPs — abort the rest of the wiring rather than mount half-broken
	// endpoints. Mount checks d.DB == nil before reaching this point;
	// MountWithFactory callers that pass nil hit the same warn-and-return.
	if f == nil {
		logger.Warn("authserver: DB not configured; skipping OAuth + login routes")
		return &Mounted{}
	}

	// DB-backed stores.
	clients := f.Clients()
	users := f.Users()
	idps := f.IdPs()
	refresh := f.Refresh()
	assignments := f.Assignments()
	federated := f.Federated()

	// In-memory stores. Janitor goroutines are owned for the process lifetime;
	// the server does not tear them down because Mount has no shutdown hook.
	pending := store.NewPendingAuthzStore()
	authcodes := d.AuthCodes
	if authcodes == nil {
		authcodes = store.NewAuthCodeStore(inMemoryStoreTTL)
	}
	bindings := store.NewBindingStore()

	// Authorize + token handlers do not depend on the local IdP and can be
	// registered regardless of its presence.
	e.GET("/oauth/authorize", oauth.AuthorizeHandler(oauth.AuthorizeDeps{
		Clients:  clients,
		Bindings: bindings,
		Pending:  pending,
		Logger:   logger,
	}))

	// Refresh helper with an optional replay hook. A refresh-token replay is
	// treated as a compromise signal: we delete the whole session chain and
	// emit a session-scoped revocation so downstream consumers kill any
	// outstanding access tokens bound to the session. The hook is skipped
	// (stays nil) when Revocation is unwired so test harnesses that only
	// exercise the token endpoint still boot.
	refreshHelper := token.NewRefreshHelper(refresh)
	if d.Revocation != nil {
		revSvc := d.Revocation
		refreshHelper.ReplayHook = func(ctx context.Context, row *store.RefreshTokenRow) error {
			sid := row.SessionID
			if err := refresh.DeleteBySessionID(ctx, sid); err != nil {
				logger.Error("authserver: replay hook: delete session chain",
					slog.String("session_id", sid), slog.Any("err", err))
			}
			if err := revSvc.Revoke(ctx, revocation.Request{
				Scope:           revocation.ScopeSession,
				TargetSessionID: &sid,
				ExpiresAt:       row.ExpiresAt,
				Reason:          revocation.ReasonReplayDetected,
			}); err != nil {
				logger.Error("authserver: replay hook: revoke session",
					slog.String("session_id", sid), slog.Any("err", err))
			}
			// Fire-and-log semantics: Rotate always surfaces ErrReplay, so
			// side-effect failures must not mutate its return contract.
			return nil
		}
	}

	// /oauth/token runs without mTLS. The agent-desktop client id additionally
	// requires a verified device cert (checked inside handleAuthCode via
	// middleware.AgentDeviceFromContext), but no optional-mTLS middleware is
	// wired on this route yet, so agent-desktop token exchanges currently
	// fail with invalid_grant. Browser clients (authorization_code for
	// non-agent clients, refresh_token) are unaffected.
	e.POST("/oauth/token", oauth.TokenHandler(oauth.TokenDeps{
		Issuer:      d.Issuer,
		Clients:     clients,
		AuthCodes:   authcodes,
		Users:       users,
		Refresh:     refreshHelper,
		Signer:      d.Signer,
		Logger:      logger,
		Assignments: assignments,
	}))

	e.POST("/oauth/introspect", oauth.IntrospectHandler(oauth.IntrospectDeps{
		Issuer:     d.Issuer,
		Keystore:   d.Keystore,
		Revocation: d.Revocation, // nil-tolerant; introspect skips
		// the revocation check when not wired (test-harness mode).
		Logger: logger,
	}))

	// /oauth/revoke (RFC 7009) requires the revocation service to record the
	// event and publish to MQ. When the caller has not wired it we skip the
	// route entirely rather than mount a handler that panics on first call.
	if d.Revocation != nil {
		e.POST("/oauth/revoke", oauth.RevokeHandler(oauth.RevokeDeps{
			Issuer:     d.Issuer,
			Keystore:   d.Keystore,
			Refresh:    refresh,
			Revocation: d.Revocation,
			Clients:    clients,
			Logger:     logger,
		}))
	} else {
		logger.Warn("authserver: Revocation not configured; skipping /oauth/revoke")
	}

	// /oauth/device-binding MUST sit behind the mTLS middleware because the
	// handler pulls the peer device out of echo context. If no lookup is
	// wired we still register the route so discovery stays honest, but it
	// will 401 on every call — document this in the log so operators know
	// the dependency is missing.
	deviceBindingHandler := oauth.DeviceBindingHandler(oauth.DeviceBindingDeps{
		Bindings: bindings,
		Logger:   logger,
	})
	if d.AgentLookup != nil {
		e.POST("/oauth/device-binding", deviceBindingHandler,
			middleware.AgentMTLSAuth(d.AgentLookup))
	} else {
		logger.Warn("authserver: AgentLookup not configured; /oauth/device-binding will reject all requests")
		e.POST("/oauth/device-binding", deviceBindingHandler)
	}

	// /authserver/password depends on the seeded local IdP UUID; without it
	// the Local adapter cannot stamp idp_id on AuthResult, so we skip the
	// password route and still register the idps endpoint so the SPA can
	// surface an empty provider list instead of failing outright.
	e.GET("/authserver/idps", login.IdpsHandler(login.IdPsDeps{
		IdPs:    idps,
		Pending: pending,
	}))

	localIdP, err := idps.GetLocal(context.Background())
	if err != nil {
		logger.Warn("authserver: local IdP not found; skipping /authserver/password",
			slog.Any("err", err))
		return &Mounted{RefreshHelper: refreshHelper}
	}

	e.POST("/authserver/password", login.PasswordHandler(login.PasswordDeps{
		Local:     idp.NewLocal(users, localIdP.ID),
		Pending:   pending,
		AuthCodes: authcodes,
		Limiter:   login.NewLimiterWithRedis(d.RedisClient),
		Audit:     d.Audit,
	}))

	// /authserver/approve completes a pending OAuth authorize request using the
	// caller's existing bearer session — the path the SPA takes when the
	// operator hits the auth dance already signed in (e.g. a loopback PKCE
	// client hitting /oauth/authorize while a CP-UI tab is open in the same
	// browser). Without this, the SPA navigates the operator away to "/" the
	// moment its status flips to authenticated and the client's loopback
	// listener hangs until it times out.
	if d.JWTVerifier != nil {
		e.POST("/authserver/approve",
			login.ApproveHandler(login.ApproveDeps{
				Pending:   pending,
				AuthCodes: authcodes,
				Audit:     d.Audit,
			}),
			d.JWTVerifier.Middleware(),
		)
	} else {
		logger.Warn("authserver: JWTVerifier not configured; skipping /authserver/approve")
	}

	// External-IdP SSO routes — registered regardless of whether any OIDC/SAML
	// IdP is currently enabled; the handlers self-check and bounce the browser
	// back to the SPA login page when the chosen provider is not usable.
	// samlRequests tracks outstanding SAML AuthnRequest IDs for InResponseTo
	// validation; its janitor is process-lifetime like the other in-memory
	// stores.
	samlRequests := store.NewSAMLRequestStore()

	// One OIDC discovery resolver shared by the SSO-start and OIDC-callback
	// legs: external IdP configs carry only the issuer (the Add-IdP form relies
	// on `.well-known/openid-configuration` discovery), so the login handlers
	// resolve authorize/token/jwks endpoints at request time. Sharing the
	// resolver means the document fetched on the start leg is a cache hit on
	// the callback leg.
	oidcResolver := oidcdisco.NewResolver()

	// Per-process HMAC signer that binds the OIDC `state` to the initiating
	// browser via a signed HttpOnly cookie (login-CSRF defense-in-depth). The
	// key is in-memory only and regenerated on restart; a restart mid-login
	// merely invalidates the in-flight cookie and the user re-initiates — an
	// acceptable failure mode that avoids any DB/Redis dependency. On the
	// (effectively impossible) CSPRNG failure we leave the signer nil so the
	// SSO legs skip the cookie rather than crash startup; the single-use,
	// 256-bit authctx + PKCE still gate the flow.
	stateSigner, signerErr := login.NewRandomStateSigner()
	if signerErr != nil {
		logger.Error("authserver: failed to mint OIDC state-cookie signer; "+
			"login-CSRF cookie binding disabled", slog.Any("err", signerErr))
		stateSigner = nil
	}

	// Unified SSO entry. The SPA login picker navigates the browser here for any
	// external IdP, regardless of protocol; StartHandler owns the OIDC-vs-SAML
	// divergence (302 to the IdP for OIDC, auto-submitting AuthnRequest POST form
	// for SAML) so the front end stays protocol-agnostic.
	startDeps := login.StartDeps{
		IdPs:        idps,
		Pending:     pending,
		Requests:    samlRequests,
		Issuer:      d.Issuer,
		Resolver:    oidcResolver,
		StateSigner: stateSigner,
	}
	e.GET("/authserver/idp/:idpId/start", login.StartHandler(startDeps))
	// RP-initiated logout — the browser navigates here after the SPA drops its
	// tokens; for an OIDC IdP it 302s to the IdP's end_session_endpoint, else to
	// /login. Reuses the start collaborators (IdPs + discovery resolver).
	e.GET("/authserver/idp/:idpId/logout", login.LogoutHandler(startDeps))

	// Return-leg handlers the external IdP redirects (OIDC) or POSTs (SAML) back
	// to once it has authenticated the user.
	oidcDeps := login.OIDCDeps{
		IdPs:        idps,
		Federated:   federated,
		Pending:     pending,
		AuthCodes:   authcodes,
		Resolver:    oidcResolver,
		StateSigner: stateSigner,
		Audit:       d.Audit,
	}
	e.GET("/authserver/oidc/callback", login.OIDCCallbackHandler(oidcDeps))

	samlDeps := login.SAMLDeps{
		IdPs:      idps,
		Federated: federated,
		Pending:   pending,
		AuthCodes: authcodes,
		Requests:  samlRequests,
		Issuer:    d.Issuer,
		Audit:     d.Audit,
	}
	e.POST("/authserver/saml/acs", login.SAMLACSHandler(samlDeps))
	e.GET("/authserver/saml/metadata", login.SAMLMetadataHandler(samlDeps))
	return &Mounted{RefreshHelper: refreshHelper}
}
