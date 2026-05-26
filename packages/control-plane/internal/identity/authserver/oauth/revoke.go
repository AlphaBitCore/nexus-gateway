package oauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// revocationService is the minimum surface RevokeHandler needs from the
// revocation package. *revocation.Service satisfies it in production; tests
// drop in a recording fake without spinning up a DB and an MQ publisher.
type revocationService interface {
	Revoke(ctx context.Context, r revocation.Request) error
}

// clientChecker is the minimum surface RevokeHandler needs from a client
// registry. *store.ClientStore satisfies it in production; tests inject a
// small in-memory fake. Mirrors the clientLoader convention in authorize.go.
type clientChecker interface {
	GetByID(ctx context.Context, id string) (*store.OAuthClient, error)
}

// RevokeDeps carries the collaborators the RFC 7009 /oauth/revoke handler
// needs. All DB-backed fields are required in production; tests may supply
// fakes that implement the same minimum method sets. A nil Clients is
// tolerated and silently skips client_id validation so unit tests can focus
// on the branch under examination.
type RevokeDeps struct {
	// Issuer is the expected iss claim on access tokens; empty disables the
	// check, which is only sensible for ad-hoc tests.
	Issuer string
	// Keystore resolves kid to RSA public key for access-token verification.
	Keystore *token.Keystore
	// Refresh is the persistent store of refresh rows. Required.
	Refresh *store.RefreshStore
	// Revocation records the revocation event and fans it out over MQ.
	// Required -- callers that pass nil will crash when Mount catches it.
	Revocation revocationService
	// Clients, when non-nil, is consulted to validate the inbound client_id.
	// Not required for correctness because the handler always defers to the
	// token's own ClientID claim / refresh row for cross-checks. Typed as an
	// interface so unit tests can plug a fake without standing up Postgres.
	Clients clientChecker
	// Logger receives non-fatal errors. Nil logs to io.Discard so handlers
	// stay safe to use without upfront wiring.
	Logger *slog.Logger
}

func (d RevokeDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// RevokeHandler implements RFC 7009 section 2.1 "Revocation Request". Per
// section 2.2, the server responds 200 OK for unknown, already-revoked, or
// mismatched tokens. This anti-enumeration rule hides token existence from
// probing callers; the only error we surface is the malformed-request case
// (missing token parameter) per section 2.1.
//
// Client auth: public clients (cp-ui, agent-desktop) send client_id in the
// form body and we accept it verbatim. Confidential-client HTTP Basic is not
// implemented because only public clients are registered today.
//
// agent-desktop cross-check: when the presented access token's device_id
// claim is set, the caller MUST also present a device cert matching that
// device_id (via middleware.AgentDeviceFromContext). This prevents a leaked
// access token on a stolen laptop from being "revoked" by the thief to cover
// tracks on other devices.
func RevokeHandler(d RevokeDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		if err := c.Request().ParseForm(); err != nil {
			return WriteOAuthError(c, ErrInvalidRequest, "malformed form body", http.StatusBadRequest)
		}
		raw := strings.TrimSpace(c.FormValue("token"))
		if raw == "" {
			return WriteOAuthError(c, ErrInvalidRequest, "token required", http.StatusBadRequest)
		}
		clientID := strings.TrimSpace(c.FormValue("client_id"))
		hint := c.FormValue("token_type_hint")

		ctx := c.Request().Context()

		// When a client registry is wired AND the caller supplied a
		// client_id, look it up. An unknown client id is indistinguishable
		// from a forged revocation attempt, so we silently 200 per RFC 7009
		// section 2.2 rather than leaking registration state. An empty
		// client_id is tolerated because some public-client flows omit it
		// when the token is self-identifying.
		if d.Clients != nil && clientID != "" {
			if _, err := d.Clients.GetByID(ctx, clientID); err != nil {
				return c.NoContent(http.StatusOK)
			}
		}
		// Per RFC 7009 section 2.1 the hint is advisory. If hint says refresh
		// we try refresh first; otherwise we try access first. Either way a
		// miss falls through to the other path, and a miss on both still
		// returns 200 so the caller cannot distinguish hit from miss.
		if hint == "refresh_token" {
			if handled := d.tryRefresh(ctx, c, raw, clientID); handled {
				return c.NoContent(http.StatusOK)
			}
			if handled := d.tryAccess(ctx, c, raw, clientID); handled {
				return c.NoContent(http.StatusOK)
			}
		} else {
			if handled := d.tryAccess(ctx, c, raw, clientID); handled {
				return c.NoContent(http.StatusOK)
			}
			if handled := d.tryRefresh(ctx, c, raw, clientID); handled {
				return c.NoContent(http.StatusOK)
			}
		}
		return c.NoContent(http.StatusOK)
	}
}

// tryRefresh returns true when the raw token matched a refresh row (whether
// or not the revocation then succeeded). A false return tells the caller to
// keep probing the other branch.
func (d RevokeDeps) tryRefresh(ctx context.Context, _ echo.Context, raw, clientID string) bool {
	hash := token.DefaultRefreshHash([]byte(raw))
	row, ok, err := d.Refresh.FindByTokenHash(ctx, hash)
	if err != nil {
		d.logger().Error("revoke: refresh lookup", slog.Any("err", err))
		// Treat as handled so we do not leak existence via a fall-through
		// timing difference against the access path.
		return true
	}
	if !ok {
		return false
	}
	// client_id mismatch still returns 200 (handled=true) so the caller
	// cannot probe which client owns a given refresh token.
	if clientID != "" && clientID != row.ClientID {
		return true
	}
	sid := row.SessionID
	if err := d.Refresh.DeleteBySessionID(ctx, sid); err != nil {
		d.logger().Error("revoke: delete by session", slog.Any("err", err))
		return true
	}
	if err := d.Revocation.Revoke(ctx, revocation.Request{
		Scope:           revocation.ScopeSession,
		TargetSessionID: &sid,
		ExpiresAt:       row.ExpiresAt, // natural tail of the session chain
		Reason:          revocation.ReasonUserLogout,
	}); err != nil {
		d.logger().Error("revoke: revocation service", slog.Any("err", err))
	}
	return true
}

// tryAccess returns true when the raw token verified as an access token we
// minted (whether or not the revocation then succeeded). A false return tells
// the caller to keep probing the other branch.
func (d RevokeDeps) tryAccess(ctx context.Context, c echo.Context, raw, clientID string) bool {
	claims, err := token.VerifyLocal(d.Keystore, d.Issuer, raw)
	if err != nil {
		// Not a token we minted; defer to the refresh branch.
		return false
	}
	// agent-desktop device binding cross-check: prevent a thief with a
	// leaked access token from revoking the legitimate owner's session.
	if claims.ClientID == "agent-desktop" && claims.DeviceID != "" {
		dev := middleware.AgentDeviceFromContext(c)
		if dev == nil || dev.ID != claims.DeviceID {
			return true
		}
	}
	if clientID != "" && clientID != claims.ClientID {
		return true
	}
	if claims.ExpiresAt == nil {
		// Malformed token without exp -- still treat as handled so we do
		// not double-dip via the refresh path.
		d.logger().Warn("revoke: access token missing exp")
		return true
	}
	jti := claims.ID
	if err := d.Revocation.Revoke(ctx, revocation.Request{
		Scope:     revocation.ScopeJTI,
		TargetJTI: &jti,
		ExpiresAt: claims.ExpiresAt.UTC(),
		Reason:    revocation.ReasonUserLogout,
	}); err != nil {
		d.logger().Error("revoke: jti revocation", slog.Any("err", err))
	}
	return true
}
