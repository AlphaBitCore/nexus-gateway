package oauth

import "github.com/labstack/echo/v4"

// OAuthError is the RFC 6749 §5.2 JSON error envelope. Status is used
// internally to pick the HTTP status code and is excluded from the body.
type OAuthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	Status      int    `json:"-"`
}

func (e *OAuthError) Error() string { return e.Code + ": " + e.Description }

// WriteOAuthError serialises an RFC 6749 §5.2 error response at the auth
// server. For authorize-endpoint failures caused by an invalid redirect_uri
// callers MUST use this (rather than reflecting via redirect) per §4.1.2.1.
func WriteOAuthError(c echo.Context, code, desc string, status int) error {
	return c.JSON(status, &OAuthError{Code: code, Description: desc, Status: status})
}

// Standard RFC 6749 §5.2 error codes used across the oauth handlers.
const (
	ErrInvalidRequest       = "invalid_request"
	ErrInvalidClient        = "invalid_client"
	ErrInvalidGrant         = "invalid_grant"
	ErrUnauthorizedClient   = "unauthorized_client"
	ErrUnsupportedGrantType = "unsupported_grant_type"
	ErrInvalidScope         = "invalid_scope"
	ErrServerError          = "server_error"
)
