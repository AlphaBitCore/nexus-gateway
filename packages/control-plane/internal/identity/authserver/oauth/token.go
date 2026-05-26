package oauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// deviceAssignmentWriter is the minimum surface TokenHandler needs to record a
// DeviceAssignment at login time. *store.AssignmentStore implements it; nil
// disables the write (non-agent clients, test harnesses without a DB).
type deviceAssignmentWriter interface {
	UpsertDeviceAssignment(ctx context.Context, p store.UpsertDeviceAssignmentParams) error
}

// Default token lifetimes. These are applied when TokenDeps.AccessTTL /
// RefreshTTL are zero so callers can wire the handler without rediscovering
// sane defaults. Values mirror the plan: 1h access, 24h refresh.
const (
	defaultAccessTTL  = time.Hour
	defaultRefreshTTL = 24 * time.Hour
)

// userLoader is the minimum surface TokenHandler needs from a user registry.
// *store.UserStore implements it; tests inject an in-memory fake.
type userLoader interface {
	GetByID(ctx context.Context, id string) (*store.User, error)
}

// refreshOps is the minimum surface the handler needs from the refresh helper.
// Factored into an interface so tests can inject a fake helper without
// depending on Postgres.
type refreshOps interface {
	NewChain(ctx context.Context, userID, clientID, deviceID string, ttl time.Duration) (string, string, string, error)
	Rotate(ctx context.Context, incoming string, ttl time.Duration) (string, string, *store.RefreshTokenRow, error)
}

// TokenDeps carries the collaborators TokenHandler needs. Tests supply a fake
// Clients loader and a fake Refresh; production wires in *store.ClientStore,
// *store.UserStore, and *token.RefreshHelper.
type TokenDeps struct {
	Issuer    string
	Clients   clientLoader
	AuthCodes *store.AuthCodeStore
	Users     userLoader
	Refresh   refreshOps
	Signer    *token.Signer
	Logger    *slog.Logger

	AccessTTL  time.Duration // 1h when zero
	RefreshTTL time.Duration // 24h when zero

	// Assignments records a DeviceAssignment row at agent-desktop token exchange
	// time (source="login"). When nil the write is skipped so non-agent clients
	// and test harnesses that do not wire a DB are unaffected.
	Assignments deviceAssignmentWriter
}

func (d TokenDeps) accessTTL() time.Duration {
	if d.AccessTTL > 0 {
		return d.AccessTTL
	}
	return defaultAccessTTL
}

func (d TokenDeps) refreshTTL() time.Duration {
	if d.RefreshTTL > 0 {
		return d.RefreshTTL
	}
	return defaultRefreshTTL
}

func (d TokenDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// tokenResponse is the RFC 6749 §5.1 success body. RefreshToken is always
// emitted because both supported grants rotate a refresh token.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// writeTokenResponse returns the §5.1 JSON body and sets Cache-Control: no-store
// per §5.1. Callers use WriteOAuthError for failure responses; both code paths
// end up with no-store, which is what §5.1 and §5.2 require.
func writeTokenResponse(c echo.Context, body tokenResponse) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Pragma", "no-cache")
	return c.JSON(http.StatusOK, body)
}

// writeTokenError wraps WriteOAuthError with the no-store headers mandated by
// RFC 6749 §5.2 for error responses issued by the token endpoint.
func writeTokenError(c echo.Context, code, desc string, status int) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Pragma", "no-cache")
	return WriteOAuthError(c, code, desc, status)
}

// TokenHandler implements the RFC 6749 §3.2 token endpoint. Only
// authorization_code and refresh_token grant types are supported;
// client_credentials and password grants are intentionally rejected with
// unsupported_grant_type so callers get a recognisable error.
func TokenHandler(d TokenDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		switch c.FormValue("grant_type") {
		case "authorization_code":
			return handleAuthCode(c, d)
		case "refresh_token":
			return handleRefresh(c, d)
		default:
			return writeTokenError(c, ErrUnsupportedGrantType, "unsupported grant_type", http.StatusBadRequest)
		}
	}
}

// handleAuthCode implements the authorization_code grant. The AuthCodeStore
// is single-use so any downstream failure after Get still leaves the code
// consumed — clients that see invalid_grant must restart the flow at
// /oauth/authorize, which is the standard-compliant behaviour.
func handleAuthCode(c echo.Context, d TokenDeps) error {
	ctx := c.Request().Context()

	code := c.FormValue("code")
	codeVerifier := c.FormValue("code_verifier")
	clientID := c.FormValue("client_id")
	redirectURI := c.FormValue("redirect_uri")
	if code == "" || codeVerifier == "" || clientID == "" || redirectURI == "" {
		return writeTokenError(c, ErrInvalidRequest, "missing required parameter", http.StatusBadRequest)
	}

	entry, ok := d.AuthCodes.Get(code)
	if !ok {
		return writeTokenError(c, ErrInvalidGrant, "unknown or expired code", http.StatusBadRequest)
	}
	if entry.ClientID != clientID {
		return writeTokenError(c, ErrInvalidGrant, "client_id mismatch", http.StatusBadRequest)
	}
	if entry.RedirectURI != redirectURI {
		return writeTokenError(c, ErrInvalidGrant, "redirect_uri mismatch", http.StatusBadRequest)
	}
	if err := VerifyPKCE(codeVerifier, entry.PKCEChallenge, "S256"); err != nil {
		return writeTokenError(c, ErrInvalidGrant, "pkce verification failed", http.StatusBadRequest)
	}

	// agent-desktop binds the access token to the mTLS peer cert. The TLS
	// layer stashes the resolved device in the Echo context; we require it
	// here and ensure its id matches the device the authorize flow locked in.
	if entry.ClientID == agentDesktopClientID {
		dev := middleware.AgentDeviceFromContext(c)
		if dev == nil {
			return writeTokenError(c, ErrInvalidGrant, "no device cert", http.StatusBadRequest)
		}
		if dev.ID != entry.DeviceID {
			return writeTokenError(c, ErrInvalidGrant, "device mismatch", http.StatusBadRequest)
		}
	}

	// Refresh chain must exist before we mint the access token so the access
	// token can carry the freshly allocated session id in "sid".
	refreshTok, sessionID, _, err := d.Refresh.NewChain(ctx, entry.UserID, entry.ClientID, entry.DeviceID, d.refreshTTL())
	if err != nil {
		d.logger().Error("token: refresh NewChain failed",
			slog.String("user_id", entry.UserID),
			slog.String("client_id", entry.ClientID),
			slog.Any("err", err),
		)
		return writeTokenError(c, ErrServerError, "refresh issuance failed", http.StatusInternalServerError)
	}

	accessTok, accessJTI, err := token.IssueAccess(d.Signer, token.AccessInput{
		Issuer:    d.Issuer,
		Subject:   entry.UserID,
		Audience:  []string{token.AdminAudience},
		ClientID:  entry.ClientID,
		Scope:     entry.Scope,
		SessionID: sessionID,
		DeviceID:  entry.DeviceID,
		Email:     entry.Email,
		IdPID:     entry.IdPID,
		AMR:       entry.AMR,
		TTL:       d.accessTTL(),
	})
	if err != nil {
		d.logger().Error("token: access mint failed", slog.Any("err", err))
		return writeTokenError(c, ErrServerError, "access issuance failed", http.StatusInternalServerError)
	}

	// Record a DeviceAssignment for the agent-desktop client so user attribution
	// is available from the moment of login (not lazily on the first heartbeat).
	// This is fire-and-forget: a write failure must never block the token response.
	if d.Assignments != nil && entry.DeviceID != "" {
		assignParams := store.UpsertDeviceAssignmentParams{
			DeviceID:    entry.DeviceID,
			UserID:      entry.UserID,
			LoginMethod: loginMethodFromAMR(entry.AMR),
			TokenJTI:    accessJTI,
			IPAddress:   c.RealIP(),
		}
		go func() {
			if err := d.Assignments.UpsertDeviceAssignment(context.Background(), assignParams); err != nil {
				d.logger().Warn("token: device assignment upsert failed",
					slog.String("device_id", assignParams.DeviceID),
					slog.String("user_id", assignParams.UserID),
					slog.Any("err", err),
				)
			}
		}()
	}

	return writeTokenResponse(c, tokenResponse{
		AccessToken:  accessTok,
		TokenType:    "Bearer",
		ExpiresIn:    int(d.accessTTL().Seconds()),
		RefreshToken: refreshTok,
		Scope:        entry.Scope,
	})
}

// loginMethodFromAMR derives the login_method string from RFC 8176 AMR values.
// "pwd" indicates local password authentication; any other value is mapped to
// "oidc" as the generic federated-IdP fallback. An empty slice means the flow
// did not carry authentication method information.
func loginMethodFromAMR(amr []string) string {
	for _, v := range amr {
		if v == "pwd" {
			return "local"
		}
	}
	if len(amr) > 0 {
		return "oidc"
	}
	return "local" // default: local auth server issued the code
}

// handleRefresh implements the refresh_token grant. The refresh row holds the
// canonical session id and user id; we reload the user to honour disabledAt
// (so disabling a user invalidates their refresh chain on next rotation) and
// to surface the current Email claim.
func handleRefresh(c echo.Context, d TokenDeps) error {
	ctx := c.Request().Context()

	refreshInput := c.FormValue("refresh_token")
	if refreshInput == "" {
		return writeTokenError(c, ErrInvalidRequest, "refresh_token required", http.StatusBadRequest)
	}

	newRefresh, _, parent, err := d.Refresh.Rotate(ctx, refreshInput, d.refreshTTL())
	if err != nil {
		switch {
		case errors.Is(err, token.ErrReplay), errors.Is(err, token.ErrExpired):
			return writeTokenError(c, ErrInvalidGrant, "invalid refresh_token", http.StatusBadRequest)
		default:
			d.logger().Error("token: refresh Rotate failed", slog.Any("err", err))
			return writeTokenError(c, ErrServerError, "refresh rotation failed", http.StatusInternalServerError)
		}
	}

	// Re-hydrate the user so a disabled account cannot extend its session by
	// rotating the refresh token, and so the Email claim is current.
	user, err := d.Users.GetByID(ctx, parent.UserID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return writeTokenError(c, ErrInvalidGrant, "user not found", http.StatusBadRequest)
		}
		d.logger().Error("token: user lookup failed",
			slog.String("user_id", parent.UserID), slog.Any("err", err))
		return writeTokenError(c, ErrServerError, "user lookup failed", http.StatusInternalServerError)
	}
	if user.DisabledAt != nil {
		return writeTokenError(c, ErrInvalidGrant, "user disabled", http.StatusBadRequest)
	}

	email := ""
	if user.Email != nil {
		email = *user.Email
	}

	var deviceID string
	if parent.DeviceID != nil {
		deviceID = *parent.DeviceID
	}

	accessTok, _, err := token.IssueAccess(d.Signer, token.AccessInput{
		Issuer:    d.Issuer,
		Subject:   user.ID,
		Audience:  []string{token.AdminAudience},
		ClientID:  parent.ClientID,
		SessionID: parent.SessionID,
		DeviceID:  deviceID,
		Email:     email,
		TTL:       d.accessTTL(),
	})
	if err != nil {
		d.logger().Error("token: access mint failed on refresh", slog.Any("err", err))
		return writeTokenError(c, ErrServerError, "access issuance failed", http.StatusInternalServerError)
	}

	return writeTokenResponse(c, tokenResponse{
		AccessToken:  accessTok,
		TokenType:    "Bearer",
		ExpiresIn:    int(d.accessTTL().Seconds()),
		RefreshToken: newRefresh,
	})
}
