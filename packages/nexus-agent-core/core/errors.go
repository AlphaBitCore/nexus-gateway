// Package core is the single component that authenticates to a Nexus
// deployment, holds the active profile, stores secrets in the OS keychain, and
// exposes typed capability functions over the existing admin API and /v1/*.
// The CLI and TUI faces are thin presenters over this package and never
// talk to HTTP, IAM, or token storage directly.
package core

import (
	"errors"
	"fmt"
)

// Sentinel error kinds. Callers classify failures with errors.Is; the concrete
// *APIError carries the detail (status, server code, IAM action).
var (
	// ErrUnauthorized is a 401 from the gateway — the credential is missing,
	// invalid, or expired and could not be refreshed.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden is a 403 — the principal authenticated but its IAM policy
	// denies the action. The *APIError carries the IAM action when present.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound is a 404 — the addressed resource does not exist.
	ErrNotFound = errors.New("not found")
	// ErrTransport is any failure that prevented a well-formed HTTP response
	// (dial error, timeout, 5xx, or an undecodable body).
	ErrTransport = errors.New("transport error")
)

// APIError is the structured failure returned by every Client call. It wraps
// one of the sentinel kinds so errors.Is(err, ErrForbidden) works, and exposes
// the server's error envelope fields for display.
type APIError struct {
	Status    int    // HTTP status code (0 when the request never completed)
	Code      string // server envelope "code" (e.g. INVALID_TOKEN)
	Type      string // server envelope "type" (e.g. authentication_error)
	Message   string // human-readable message
	IAMAction string // the denied IAM action, when the server reports one (403)
	kind      error  // the sentinel this wraps
}

func (e *APIError) Error() string {
	if e.IAMAction != "" {
		return fmt.Sprintf("%s (%d %s): %s [iam: %s]", e.kind, e.Status, e.Code, e.Message, e.IAMAction)
	}
	if e.Status == 0 {
		return fmt.Sprintf("%s: %s", e.kind, e.Message)
	}
	return fmt.Sprintf("%s (%d %s): %s", e.kind, e.Status, e.Code, e.Message)
}

// Unwrap exposes the sentinel kind so errors.Is classifies the failure.
func (e *APIError) Unwrap() error { return e.kind }

// kindForStatus maps an HTTP status to its sentinel kind.
func kindForStatus(status int) error {
	switch status {
	case 401:
		return ErrUnauthorized
	case 403:
		return ErrForbidden
	case 404:
		return ErrNotFound
	default:
		return ErrTransport
	}
}
