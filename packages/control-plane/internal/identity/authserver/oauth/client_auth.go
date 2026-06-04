package oauth

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// clientAuthResult describes the outcome of verifyClientAuth. On success
// Client is non-nil and ErrCode is empty. On failure ErrCode + Desc + Status
// are populated for direct hand-off to writeTokenError.
type clientAuthResult struct {
	Client  *store.OAuthClient
	ErrCode string
	Desc    string
	Status  int
}

// verifyClientAuth authenticates the calling client per RFC 6749 §2.3 + §5.2.
//
// Confidential clients MUST present client_secret via either:
//   - client_secret_basic — HTTP Basic Authorization header (RFC 6749 §2.3.1
//     preferred form)
//   - client_secret_post  — request-body form field client_secret
//
// Public clients MUST NOT present a secret; doing so is rejected so a
// misconfigured caller fails loudly instead of silently downgrading to no
// authentication. (RFC 6749 §2.3.1 leaves this implementation-defined; we
// pick reject because the admin UI in this repo always pairs type=public
// with secretless creation.)
//
// Returns a clientAuthResult. Callers should check ErrCode != "" and bail
// via writeTokenError on failure.
func verifyClientAuth(ctx context.Context, c echo.Context, clients clientLoader, clientID string) clientAuthResult {
	if clients == nil {
		// Wiring shape that omits Clients cannot authenticate any caller —
		// fail closed rather than mint tokens.
		return clientAuthResult{
			ErrCode: ErrServerError,
			Desc:    "client loader not configured",
			Status:  http.StatusInternalServerError,
		}
	}

	client, err := clients.GetByID(ctx, clientID)
	if err != nil {
		return clientAuthResult{
			ErrCode: ErrInvalidClient,
			Desc:    "unknown client_id",
			Status:  http.StatusUnauthorized,
		}
	}

	suppliedSecret := extractClientSecret(c)

	switch client.Type {
	case "confidential":
		if suppliedSecret == "" {
			return clientAuthResult{
				ErrCode: ErrInvalidClient,
				Desc:    "client_secret required for confidential client",
				Status:  http.StatusUnauthorized,
			}
		}
		if client.ClientSecretHash == nil || *client.ClientSecretHash == "" {
			// Misconfigured row: type=confidential but no stored hash.
			// Treat as auth failure rather than letting any secret pass.
			return clientAuthResult{
				ErrCode: ErrInvalidClient,
				Desc:    "confidential client missing stored secret",
				Status:  http.StatusUnauthorized,
			}
		}
		if err := bcrypt.CompareHashAndPassword(
			[]byte(*client.ClientSecretHash),
			[]byte(suppliedSecret),
		); err != nil {
			return clientAuthResult{
				ErrCode: ErrInvalidClient,
				Desc:    "client_secret mismatch",
				Status:  http.StatusUnauthorized,
			}
		}

	case "public":
		if suppliedSecret != "" {
			return clientAuthResult{
				ErrCode: ErrInvalidClient,
				Desc:    "public client must not present client_secret",
				Status:  http.StatusUnauthorized,
			}
		}

	default:
		return clientAuthResult{
			ErrCode: ErrInvalidClient,
			Desc:    "unsupported client type",
			Status:  http.StatusUnauthorized,
		}
	}

	return clientAuthResult{Client: client}
}

// extractClientSecret pulls client_secret per RFC 6749 §2.3.1. The HTTP Basic
// Authorization header takes precedence — its presence indicates the caller
// chose client_secret_basic, and §2.3.1 forbids combining the two methods.
func extractClientSecret(c echo.Context) string {
	auth := c.Request().Header.Get("Authorization")
	if strings.HasPrefix(auth, "Basic ") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err == nil {
			parts := strings.SplitN(string(raw), ":", 2)
			if len(parts) == 2 {
				return parts[1]
			}
		}
	}
	return c.FormValue("client_secret")
}
