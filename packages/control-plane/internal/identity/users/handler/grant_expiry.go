package iam

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// parseGrantExpiry reads a caller-supplied `expiresAt` off a request body and
// REFUSES anything it cannot use, instead of falling back to "no expiry".
//
// An empty string means the caller did not ask for an expiry, and returns
// (nil, nil). Anything else must be RFC3339 and must be in the future.
//
// This exists because the three admin API-key minting paths disagreed about the
// same field. Rotate refused a malformed value; create and update discarded it
// and carried on — so `expiresAt: "2026-13-45"`, or a JS `Date` serialized any
// way but RFC3339, minted a PERMANENT admin API key and answered 201. The key
// authenticates as its owner, so the field silently dropped there is the only
// thing bounding a credential's lifetime.
//
// The past-expiry check is the second half. Every other expiry-bearing grant in
// this service already rejects a past timestamp — IAM policy attachment
// (identity/users/handler/iam_attachment_handlers.go), device-group membership
// (fleet/handler/agent/groups.go) — and the compliance-exemption endpoint
// refuses a computed expiry that is not in the future. The API-key paths were
// the outliers accepting one, which reads as "expired on arrival": the key is
// live from the moment it is minted until something else notices.
//
// Refusal is reported by the BOOLEAN, never by the error. `c.JSON` returns nil
// when the write succeeds, so a guard that signals "refused" by handing back its
// result has a dead branch at every call site — the 400 goes out and the caller
// carries on and mints the key anyway. That is the same shape as the SCIM group
// guard that answered 403 and performed the mutation. The contract here:
//
//	t, ok, err := parseGrantExpiry(c, raw)
//	if !ok { return err }   // response already written; err is only a write failure
func parseGrantExpiry(c echo.Context, raw string) (*time.Time, bool, error) {
	if raw == "" {
		return nil, true, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, false, c.JSON(http.StatusBadRequest,
			errJSON("expiresAt must be RFC3339", "validation_error", "INVALID_EXPIRES_AT"))
	}
	if !t.After(time.Now()) {
		return nil, false, c.JSON(http.StatusBadRequest,
			errJSON("expiresAt must be in the future", "validation_error", "EXPIRES_AT_IN_PAST"))
	}
	return &t, true, nil
}
