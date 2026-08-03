package vkauth

// recheck.go — by-hash VK status re-validation for long-lived sessions.
//
// A WebSocket realtime session can outlive a VK revocation by more than an
// hour; per-request Authenticate cannot help because a session authenticates
// exactly once, at upgrade. The session therefore retains ONLY the non-secret
// matched key hash (see AuthenticateWithHash) and calls RecheckByHash on a
// timer. The contract distinguishes a DEFINITIVE negative (sever the session)
// from an INDETERMINATE lookup failure (fail open — a transient backend blip
// must not simultaneously kill every live session).

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecheckOutcome is the result of a by-hash VK status re-validation.
type RecheckOutcome int

const (
	// RecheckActive — the VK row exists, is enabled, unexpired, and in
	// active status. The session continues.
	RecheckActive RecheckOutcome = iota
	// RecheckRevoked — a definitive negative: the row is gone (deleted),
	// disabled, expired, or in a non-active status. The caller severs.
	RecheckRevoked
)

// RecheckByHash re-validates a VK by its stored HMAC hash through the same
// lookup surface Authenticate uses (production wires the cache layer, which
// the Hub invalidates near-instantly on VK changes; the bounded cache TTL is
// the fallback).
//
// Return contract:
//   - (RecheckRevoked, nil) — definitive negative: row deleted (no-rows),
//     disabled, expired, or non-active status. Sever.
//   - (RecheckActive, nil)  — the VK is still valid. Continue.
//   - (RecheckActive, err)  — indeterminate lookup failure (DB/cache blip).
//     The caller MUST fail open and retry on its next tick; treating a
//     transient error as a revocation would mass-sever healthy sessions.
func (a *Authenticator) RecheckByHash(ctx context.Context, keyHash string) (RecheckOutcome, error) {
	vk, err := a.db.GetVirtualKeyByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RecheckRevoked, nil // row deleted — definitive
		}
		return RecheckActive, err // indeterminate — caller fails open
	}
	if vk == nil {
		return RecheckRevoked, nil // lookup surfaces a miss as nil row
	}
	if !vk.Enabled {
		return RecheckRevoked, nil
	}
	if vk.ExpiresAt != nil && vk.ExpiresAt.Before(time.Now()) {
		return RecheckRevoked, nil
	}
	if vk.VKStatus != nil && *vk.VKStatus != "" && *vk.VKStatus != "active" {
		return RecheckRevoked, nil
	}
	return RecheckActive, nil
}
