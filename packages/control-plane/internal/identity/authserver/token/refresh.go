package token

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// ErrReplay signals that a refresh token has already been consumed (its
// RefreshToken row has a non-nil usedAt, or the row is unknown). Callers at
// the token endpoint MUST translate this to RFC 6749 invalid_grant; the
// session-revocation layer treats it as a compromise signal.
var ErrReplay = errors.New("refresh: replay or unknown token")

// ErrExpired signals that a refresh token's expiresAt is in the past. The
// caller maps this to invalid_grant; replay vs expiry is deliberately
// distinguished so telemetry can tell them apart.
var ErrExpired = errors.New("refresh: expired")

// refreshHashFn hashes raw opaque tokens before storage. SHA-256 is sufficient
// because the token itself is 32 bytes from crypto/rand — the hash defends
// against DB dump exposure, not against preimage attacks on short secrets.
type refreshHashFn func([]byte) []byte

// RefreshStoreIface is the minimum subset of *store.RefreshStore that
// RefreshHelper needs. Declaring it as an interface lets unit tests
// substitute fakes without standing up a Postgres instance, while keeping
// the production constructor signature unchanged (*store.RefreshStore
// satisfies the interface implicitly). Mirrors the PgxPool convention in
// packages/control-plane/internal/authserver/revocation/store.go.
type RefreshStoreIface interface {
	Insert(ctx context.Context, row *store.RefreshTokenRow) error
	FindByTokenHash(ctx context.Context, hash []byte) (*store.RefreshTokenRow, bool, error)
	MarkUsed(ctx context.Context, jti string) (bool, error)
}

// DefaultRefreshHash implements refreshHashFn with SHA-256. Exposed publicly
// so callers outside the token package (notably the /oauth/revoke handler)
// can compute the same hash the helper writes to storage without duplicating
// the algorithm choice.
func DefaultRefreshHash(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// ReplayHook is invoked by Rotate whenever it returns ErrReplay against a
// known parent row (either the row is already used or the MarkUsed race is
// lost). Callers wire this to session-revocation side-effects: a replay is a
// compromise signal, so the entire refresh chain plus any outstanding access
// tokens tied to that session must be torn down.
//
// The hook receives the parent row so the caller can read SessionID, UserID,
// etc. without an extra DB roundtrip. Errors returned by the hook are ignored
// by Rotate -- hook failures must not mask or replace the ErrReplay surfaced
// to the /oauth/token handler, and callers that need visibility should log
// inside the hook itself (the helper intentionally has no logger dependency).
//
// The hook is NOT invoked on the "unknown token hash" branch because there is
// no parent row to scope the revocation to; inventing one would pollute the
// revocation stream with phantom sessions.
type ReplayHook func(ctx context.Context, row *store.RefreshTokenRow) error

// RefreshHelper issues and rotates refresh tokens on top of *store.RefreshStore.
// The helper owns the hashing convention and the opaque-token format so every
// caller uses the same bytes on both sides of the Get/MarkUsed boundary.
type RefreshHelper struct {
	// Store is typed as the RefreshStoreIface interface so unit tests can
	// swap in fakes. Production callers pass the concrete *store.RefreshStore
	// via NewRefreshHelper; the field is exported so the historical
	// zero-value initialiser pattern still works.
	Store  RefreshStoreIface
	HashFn refreshHashFn // defaults to SHA-256 when nil
	// ReplayHook, when non-nil, fires on every ErrReplay branch that has a
	// known parent row. Nil is tolerated so unit tests that exercise only
	// single-use semantics do not need to wire a stub.
	ReplayHook ReplayHook
}

// NewRefreshHelper returns a RefreshHelper bound to s using the default hash.
func NewRefreshHelper(s *store.RefreshStore) *RefreshHelper {
	return &RefreshHelper{Store: s}
}

// fireReplay invokes ReplayHook when configured and always returns ErrReplay.
// The hook is called synchronously so it shares the caller's request context
// and deadline; moving it to a goroutine would outlive the HTTP request and
// break correlation with the token request trace.
func (h *RefreshHelper) fireReplay(ctx context.Context, row *store.RefreshTokenRow) error {
	if h.ReplayHook != nil {
		// Hook errors are deliberately swallowed: callers must always see a
		// consistent ErrReplay regardless of side-effect failures, and the
		// hook itself is responsible for logging. See ReplayHook godoc.
		_ = h.ReplayHook(ctx, row)
	}
	return ErrReplay
}

func (h *RefreshHelper) hash(b []byte) []byte {
	if h.HashFn != nil {
		return h.HashFn(b)
	}
	return DefaultRefreshHash(b)
}

// NewChain mints the first refresh token for a new login session. It allocates
// a fresh sessionID and jti, inserts a row with ParentJTI="" (indicating the
// chain root), and returns the raw opaque token alongside its correlation ids.
// The raw token is never persisted; only its SHA-256 hash goes to the DB.
func (h *RefreshHelper) NewChain(ctx context.Context, userID, clientID, deviceID string, ttl time.Duration) (string, string, string, error) {
	raw := store.RandomOpaqueToken(32)
	sid := uuid.NewString()
	jti := uuid.NewString()

	var devicePtr *string
	if deviceID != "" {
		d := deviceID
		devicePtr = &d
	}

	now := time.Now()
	row := &store.RefreshTokenRow{
		JTI:       jti,
		SessionID: sid,
		ParentJTI: "",
		UserID:    userID,
		ClientID:  clientID,
		DeviceID:  devicePtr,
		TokenHash: h.hash([]byte(raw)),
		ExpiresAt: now.Add(ttl).UTC(),
	}
	if err := h.Store.Insert(ctx, row); err != nil {
		return "", "", "", err
	}
	return raw, sid, jti, nil
}

// Rotate validates an incoming raw refresh token and, on success, marks the
// old row used and inserts a new row linked via ParentJTI. The new row inherits
// sessionID, userID, clientID, and deviceID from the parent so the refresh
// chain preserves session identity across rotations.
//
// Returns (rawToken, newJTI, parentRow, nil) on success. parentRow lets the
// caller read sessionID/userID/clientID/deviceID without a second DB roundtrip.
//
// Error semantics:
//   - ErrReplay: token hash unknown, or row already has usedAt (replay).
//     Callers MUST treat this as invalid_grant and SHOULD revoke the session.
//   - ErrExpired: row exists and is fresh but expiresAt is in the past.
func (h *RefreshHelper) Rotate(ctx context.Context, incoming string, ttl time.Duration) (string, string, *store.RefreshTokenRow, error) {
	hash := h.hash([]byte(incoming))
	row, found, err := h.Store.FindByTokenHash(ctx, hash)
	if err != nil {
		return "", "", nil, err
	}
	if !found {
		return "", "", nil, ErrReplay
	}
	if row.UsedAt != nil {
		// Already rotated -- treat as replay. Intentionally classified as
		// replay rather than ErrExpired so callers can distinguish
		// "honest expiry" from "possibly compromised token". The parent row
		// is the authoritative compromise anchor; hand it to the hook so the
		// caller can revoke the full session chain.
		return "", "", nil, h.fireReplay(ctx, row)
	}
	if time.Now().After(row.ExpiresAt) {
		return "", "", nil, ErrExpired
	}

	// Atomicity boundary: MarkUsed flips usedAt iff NULL. Two concurrent
	// Rotate calls for the same token will race here; the loser sees (false,
	// nil) and surfaces ErrReplay. The loser's view of the parent row is the
	// same compromise anchor as the used-already branch, so we fire the hook
	// with it.
	ok, err := h.Store.MarkUsed(ctx, row.JTI)
	if err != nil {
		return "", "", nil, err
	}
	if !ok {
		return "", "", nil, h.fireReplay(ctx, row)
	}

	raw := store.RandomOpaqueToken(32)
	newJTI := uuid.NewString()
	newRow := &store.RefreshTokenRow{
		JTI:       newJTI,
		SessionID: row.SessionID,
		ParentJTI: row.JTI,
		UserID:    row.UserID,
		ClientID:  row.ClientID,
		DeviceID:  row.DeviceID,
		TokenHash: h.hash([]byte(raw)),
		ExpiresAt: time.Now().Add(ttl).UTC(),
	}
	if err := h.Store.Insert(ctx, newRow); err != nil {
		return "", "", nil, err
	}
	return raw, newJTI, row, nil
}
