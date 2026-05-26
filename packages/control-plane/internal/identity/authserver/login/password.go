package login

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/idp"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

const authCodeTTL = 5 * time.Minute

// RateLimiter is the minimal surface the password handler needs. Taking an
// interface lets tests inject a deterministic limiter without reaching
// into the concrete type.
type RateLimiter interface {
	Allow(ip, email string) bool
}

// PasswordDeps carries the collaborators the password-submission handler
// needs. The handler no longer re-renders HTML on failure, so the IdP store
// is no longer required.
type PasswordDeps struct {
	Local     *idp.Local
	Pending   *store.PendingAuthzStore
	AuthCodes *store.AuthCodeStore
	Limiter   RateLimiter
	// Audit emits AdminAuditLog rows for admin.login.{failed,succeeded}.
	// nil disables emission (test harnesses).
	Audit *audit.Writer
}

// PasswordSubmitRequest is the JSON body accepted by POST /authserver/password.
type PasswordSubmitRequest struct {
	AuthCtx  string `json:"authctx"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// PasswordSubmitResponse is the success payload: the SPA navigates the
// browser to `redirectUri` to hand the OAuth code off to the client.
type PasswordSubmitResponse struct {
	RedirectURI string `json:"redirectUri"`
}

// errorResponse matches the AuthserverError schema in the OpenAPI spec.
type errorResponse struct {
	Error string `json:"error"`
}

const (
	errInvalidCredentials = "invalid_credentials"
	errUserDisabled       = "user_disabled"
	errAuthctxExpired     = "authctx_expired"
	errRateLimited        = "rate_limited"
	errInternal           = "internal_error"
)

// PasswordHandler returns an echo.HandlerFunc that processes POST
// /authserver/password. On success it mints an authorization code, binds it
// to the pending authorize entry, and returns the fully-formed redirect URI
// the SPA should navigate to. Failures return a typed JSON error code so
// the SPA can render an inline message without reloading the page.
func PasswordHandler(d PasswordDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req PasswordSubmitRequest
		if err := c.Bind(&req); err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}

		if !d.Limiter.Allow(c.RealIP(), req.Email) {
			return c.JSON(http.StatusTooManyRequests, errorResponse{Error: errRateLimited})
		}

		result, err := d.Local.Authenticate(c.Request().Context(), map[string]string{
			"email":    req.Email,
			"password": req.Password,
		})
		if err != nil {
			code := errInvalidCredentials
			if errors.Is(err, idp.ErrUserDisabled) {
				code = errUserDisabled
			}
			// Emit audit row so brute-force / leaked-key detection can fire on the
			// AdminAuditLog stream (consumed by the auth.login_failure_rate
			// alert rule). ActorLabel is the email as typed; we deliberately do
			// NOT look up whether the user exists, to preserve timing-safety.
			if d.Audit != nil {
				d.Audit.LogObserved(c.Request().Context(), audit.Entry{
					Action:     "admin.login.failed",
					ActorLabel: req.Email,
					SourceIP:   c.RealIP(),
				})
			}
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: code})
		}
		// Emit success-side audit row for symmetry + future SLO use.
		if d.Audit != nil {
			d.Audit.LogObserved(c.Request().Context(), audit.Entry{
				Action:     "admin.login.succeeded",
				ActorLabel: result.Email,
				ActorID:    result.UserID,
				SourceIP:   c.RealIP(),
				EntityType: "user",
				EntityID:   result.UserID,
			})
		}

		pending, ok := d.Pending.Take(req.AuthCtx)
		if !ok {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}

		code := store.RandomOpaqueToken(32)
		d.AuthCodes.Put(code, store.AuthCodeEntry{
			ClientID:      pending.ClientID,
			UserID:        result.UserID,
			RedirectURI:   pending.RedirectURI,
			PKCEChallenge: pending.CodeChallenge,
			Scope:         pending.Scope,
			IdPID:         result.IdPID,
			DeviceID:      pending.DeviceID,
			Nonce:         pending.Nonce,
			Email:         result.Email,
			AMR:           result.AMR,
			ExpiresAt:     time.Now().Add(authCodeTTL),
		})

		redirect, err := buildRedirect(pending.RedirectURI, code, pending.State)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		return c.JSON(http.StatusOK, PasswordSubmitResponse{RedirectURI: redirect})
	}
}

// buildRedirect appends the code and state query parameters to the
// registered redirect URI without mutating scheme/host/path. Using net/url
// keeps the emitted URI byte-identical to the registered one apart from the
// appended parameters, so OAuthClient.RedirectAllowed still matches on the
// callback side.
func buildRedirect(redirectURI, code, state string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
