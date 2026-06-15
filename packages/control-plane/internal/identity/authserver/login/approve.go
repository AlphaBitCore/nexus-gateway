package login

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// ApproveDeps carries the collaborators the approve-from-session handler
// needs. It mirrors PasswordDeps with the password fields removed — there is
// no IdP authentication because the bearer middleware in front of this
// handler has already verified the caller's session.
type ApproveDeps struct {
	Pending   *store.PendingAuthzStore
	AuthCodes *store.AuthCodeStore
	Audit     *audit.Writer
}

// ApproveRequest is the JSON body accepted by POST /authserver/approve.
type ApproveRequest struct {
	AuthCtx string `json:"authctx"`
}

// ApproveResponse mirrors PasswordSubmitResponse so the SPA can navigate the
// browser to redirectUri uniformly across login paths.
type ApproveResponse struct {
	RedirectURI string `json:"redirectUri"`
}

// ApproveHandler completes a pending OAuth authorize request using the
// caller's existing session (verified by the bearer middleware in front of
// it). The SPA invokes this when the operator arrives at /login?authctx=…
// already signed in — the dance no longer goes through the password form.
//
// Without this endpoint the SPA navigates the operator to "/" the moment
// status becomes authenticated, dropping the OAuth flow and leaving a
// loopback PKCE client's listener hanging until it times out.
func ApproveHandler(d ApproveDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		claims := jwtverifier.ClaimsFrom(c)
		if claims == nil || claims.Subject == "" {
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: errInternal})
		}
		var req ApproveRequest
		if err := c.Bind(&req); err != nil || req.AuthCtx == "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}
		pending, ok := d.Pending.Take(req.AuthCtx)
		if !ok {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}
		code := store.RandomOpaqueToken(32)
		d.AuthCodes.Put(code, store.AuthCodeEntry{
			ClientID:      pending.ClientID,
			UserID:        claims.Subject,
			RedirectURI:   pending.RedirectURI,
			PKCEChallenge: pending.CodeChallenge,
			Scope:         pending.Scope,
			IdPID:         claims.IDP,
			DeviceID:      pending.DeviceID,
			Nonce:         pending.Nonce,
			Email:         claims.Email,
			AMR:           claims.AMR,
			ExpiresAt:     time.Now().Add(authCodeTTL),
		})
		redirect, err := buildRedirect(pending.RedirectURI, code, pending.State)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		if d.Audit != nil {
			d.Audit.LogObserved(c.Request().Context(), audit.Entry{
				Action:     "admin.login.succeeded",
				ActorLabel: claims.Email,
				ActorID:    claims.Subject,
				SourceIP:   c.RealIP(),
				EntityType: "user",
				EntityID:   claims.Subject,
			})
		}
		return c.JSON(http.StatusOK, ApproveResponse{RedirectURI: redirect})
	}
}
